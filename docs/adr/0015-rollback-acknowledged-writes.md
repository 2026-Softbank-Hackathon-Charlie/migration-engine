# 0015. 정상 복귀의 최종 상태 검증과 환경 장애 내구성을 구분한다

- 상태: 채택
- 날짜: 2026-11-06
- 관련: migration-engine #19, ADR 0003, 0013

## 상황

역방향 이전은 살아 있는 새 primary를 강등하고 마지막 WAL을 따라잡은 옛 환경을 승격한다. 정상 복귀에서 acknowledged write 유실은 필연이 아니다. 반면 비동기 복제 중 primary 환경과 최신 WAL을 함께 잃으면 복구할 수 없다. PK 존재 검사만으로 UPDATE와 DELETE의 보존을 증명할 수 없다.

## 결정

- `verify.writtenRecords`의 기존 table/pk 형식을 유지하고 `expected`(전체 행 JSON 객체) 또는 `absent:true`(삭제 후 부재)를 추가한다. 둘은 함께 지정할 수 없다. 검증할 테이블에는 PK가 필요하다. 문자열 식별자는 인용하고 PK와 JSON은 SQL 매개변수로 전달한다.
- 독립 클라이언트 기록에는 요청 ID, 성공 응답 여부, 트랜잭션 ID와 최종 상태를 보관한다. 결과 불명 요청은 성공으로 세지 않는다. 검증 파일은 writer 정지 후 키마다 마지막으로 성공한 상태 하나를 내보낸다. expected에는 개인정보가 포함될 수 있으므로 원본 파일은 비공개로 보관한다.
- 최종 상태 검사는 동일한 키를 후속 작업이 바꾼 후에는 과거 상태를 검사하지 않는다. 중복 부작용은 앱의 고유 작업 ID 제약과 별도 작업 원장으로 검증해야 한다. 이 확장은 exactly-once 실행 보장이 아니다.
- gateway 바깥 배치·관리자·직접 DB writer는 운영자가 중지하고 새 연결을 네트워크/권한 정책으로 차단한 뒤 진행 중 트랜잭션 종료를 확인해야 한다. 그 증거가 없으면 이전을 시작하지 않는다. 엔진의 gateway fence만으로 외부 writer를 통제했다고 주장하지 않는다.
- fence, switchover, start-target, cutover 실행 중 프로세스가 종료되면 저장된 running 단계를 자동 재실행하지 않고 halt한다. gateway를 자동 해제하거나 오래된 replica를 강제 승격하지 않는다. 승격 후 명시적 ResumeHalted의 기존 실상태 검사는 유지한다.
- 리포트에 비동기 내구성 범위를 명시한다. rollback window는 옛 원본과 WAL 복제가 유지되고 decommission 전인 기간이다. 고정된 시간 보장은 없다. 정상 복귀의 RTO는 실제 catch-up/강등/승격/안정 대기/앱 기동에 의존하며 타임아웃 시 안전 중단한다.

## 검토한 대안

- HTTP 요청 재전송/캐시: DB commit WAL의 내구성을 대체하지 못한다.
- 로컬 동기 replica: 대상 내부 노드 장애 보호이며 대상 환경 전체 유실 보호와 다르다.
- 교차 환경 동기 모드: CNPG 공식 replication 문서는 `postgresql.synchronous`로 외부 standby 이름을 포함할 수 있다고 설명한다. 하지만 설치 버전, 실제 walreceiver application_name, 승격 후 역할 변경, synchronous_commit의 원격 flush 조건을 확인해야 한다. required 원격 복사본이 없어졌을 때 비동기로 fallback하면 환경 장애 RPO=0이 아니다. 왕복 리허설과 원격 단절·primary 저장소 유실 검증 전까지 이 제품의 RPO=0 모드는 미지원이다.

## 결과

일반 WAL 기록 테이블과 gateway 통제 writer의 계획된 복귀를 대상으로 한다. UNLOGGED/임시 테이블, 외부 결제·이메일, 스키마 역변환, 논리적 오류 취소는 제외한다. 동기 모드를 활성화하는 운영 변경은 하지 않는다. 네트워크 단절 시 강제 승격 없이 중단한다. 실제 PG/CNPG 왕복·지연·장애 주입 검증이 완료되어야 이슈의 live 완료 기준을 충족한다.

공식 자료: https://cloudnative-pg.io/docs/1.27/replication/ 및 https://cloudnative-pg.io/documentation/current/replica_cluster/ (설치 버전에 대한 별도 확인 필요)
