# 0017. 승격 안정화의 DB 신호를 단일 SQL로 관측한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

쓰기 보류 중 `waitSettled`는 매 관측마다 CNPG 상태를 읽고 새 DB 연결을 열어
`pg_is_in_recovery`, `pg_settings.pending_restart`, `pg_postmaster_start_time`을
세 SQL로 순서대로 읽었다. `pg.Connect`의 Ping도 별도로 존재한다. 지연이 있는
DB 경로에서는 안정화 조건이 충족되어도 이 왕복들이 추가 대기를 만든다.

과거 CNPG 리허설의 안정화 대기 전체와 이 관측 비용은 구분해야 한다. 서버 재시작,
CNPG 상태 반영 및 루프 대기가 남아 있으므로 SQL 개선이 전체 보류 시간을 없애지는 않는다.

## 결정

`pg.DB.SettleStatus`는 같은 세 신호를 하나의 SELECT로 반환한다. 엔진은 비공개
optional interface로 이를 선택하며 기존 `Database` 인터페이스와 다른 adapter의
세 SQL 경로는 보존한다. 단일 관측의 오류는 미관측으로 처리하고 개별 SQL로 재시도해
오류를 숨기지 않는다.

매 관측의 새 연결과 Close, 동일 서버 프로세스에서 최소 두 번의 안정 관측,
기존 `settleMin`, 상태 오류 처리와 승격 이후 자동 원복 금지는 유지한다.

`SettleReport`에는 `clusterReadMillis`, `connectMillis`, `databaseReadMillis`,
`closeMillis`, 합산 `observationMillis`를 추가한다. 성공·실패 관측의 원래 duration을
누적한 뒤 ms로 변환하여 각 poll의 반올림 손실을 피한다. 합산값과 개별 ms 합에는
최종 절삭으로 몇 ms 차이가 있을 수 있다. 이 값은 종료 시 저장되는 증거이며 실시간
증가 타이머가 아니다. poll sleep이나 CNPG 자체 재시작 시간도 직접 나타내지 않는다.

## 검토한 대안

- 모든 adapter에 새 메서드 요구: 기존 구현을 깨므로 선택적 확장을 사용한다.
- 세 쿼리 병렬화: 추가 연결 및 DB 부하를 만들므로 단일 SELECT를 선택한다.
- 동일 DB 연결 계속 유지: CNPG smart shutdown을 지연할 수 있어 배제한다.
- 안정화 시간·관측 신호 축소: 재시작과 pending restart를 놓칠 위험이 있어 변경하지 않는다.

## 결과

실제 격리 PostgreSQL 17.11에서 교대 A/B로 모든 신호가 동일함을 확인했다. Linux
Go 1.26.5의 8회씩 측정에서 각 방향 TCP forwarding 지연 10ms일 때 DB 쿼리 중앙값은
127.82→43.82ms, 연결/Ping/Close 포함 217.69→133.39ms였다. 25ms 조건에서는
308.05→104.06ms, 전체 517.71→313.69ms였다.

지연은 각 TCP read를 전달하기 전에 인위적으로 넣었다. 이는 특정 클라우드의 정확한 RTT나
운영 이전 전체 개선률의 증거가 아니다. 실제 DB 쿼리를 사용했으며 CNPG promotion/restart를
흉내 낸 결과도 아니다. 자세한 방법과 원시 샘플은
[측정 기록](../test-records/2026-11-07-settle-probe/README.md)에 있다.

실제 CNPG 이전 전체의 보류 시간 변화와 운영 배포 효과는 별도 리허설이 필요하다.
