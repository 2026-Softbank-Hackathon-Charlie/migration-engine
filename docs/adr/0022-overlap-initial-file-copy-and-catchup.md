# 0022. 초기 파일 복사와 DB 따라잡기를 병렬 실행하되 두 작업이 끝나야 진행한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

presync는 초기 DB 복제 지연이 기준 이하로 내려오기를 기다린 뒤 파일 복사를 시작했다. 두 작업은 서로의 결과를 사용하지 않으며, 이 시점에는 쓰기가 허용된다. 직렬 대기 때문에 두 작업의 시간이 합산됐다.

기존 운영 기록에서는 파일 복사가 주요 대기였다. 별도로 AI 리허설의 prepare가 오래 걸렸지만 기존 단계 기록으로는 manifest 적용·클러스터 준비·DB 연결 대기를 구분하지 못했다.

## 결정

- 파일이 있는 presync에서 초기 catchup과 파일 복사를 고정 2개 작업으로 병렬 실행한다. DB 접근은 catchup 작업만 하며 파일 adapter는 다른 작업에서 한 번 호출한다. 공유 evidence map은 두 작업이 종료된 뒤 작성한다.
- 하나라도 실패하면 공통 context를 취소하고 두 작업 종료를 기다린다. 실패 원인은 합쳐 보존하고, 실행 중인 전송을 남겨 둔 채 checkpoint·retry·DB Close로 진행하지 않는다.
- delta의 파일 복사 뒤 최신 lag 확인, fence의 새 파일 스냅샷·최종 LSN, fence gate, promotion·settle 순서는 보존한다. presync가 최종 쓰기 경계를 증명한다고 취급하지 않는다.
- presync에는 catchup·files·전체 소요를 별도로 기록한다. 겹친 작업의 구간 시간은 합산하지 않는다.
- prepare에 실제 실행한 targetLookup, manifestApply, targetHealthy, sourceTopology, targetRecovery, sourceRewire, stopTarget 구간과 전체 시간을 기록한다. targetRecovery는 연결 재시도와 recovery 상태 읽기를 합친 시간이다. 실패한 구간도 evidence와 metric에 남기며 실행하지 않은 구간을 0으로 만들지 않는다.

## 검토한 대안

- 기존 직렬 실행: 초기 복제와 파일 복사의 합산 대기가 유지된다.
- delta/fence/프로모션까지 병렬화: 최종 WAL·파일의 정합성과 단일 primary 경계에 의존하므로 제외했다.
- target manifest를 검증 전 미리 실행: 샌드박스 격리와 승인 순서를 바꾸므로 제외했다.
- 모든 테이블·파일 검사를 병렬화: DB·스토리지 부하를 별도로 검토해야 하므로 이번 변경에 포함하지 않았다.

## 결과

채널 barrier 테스트로 두 초기 작업이 서로를 기다리지 않고 시작하는지 검증한다. 양방향 실패 취소, 모든 작업 종료 후 반환, 쓰기 보류·프로모션 플래그 미변경을 검증한다. prepare 실패 시 구간 기록도 검사한다.

이론상 presync 시간은 두 작업의 합에서 큰 작업의 시간에 가까워진다. 실제 기록처럼 DB가 이미 따라잡았다면 이 부분의 이득은 작다. 운영 이전 시간을 재측정하지 않았으므로 단축률을 주장하지 않는다. `PromoteRequested` 이후 자동 DB 롤백 금지와 기존 DB 부하 예산을 유지한다.
