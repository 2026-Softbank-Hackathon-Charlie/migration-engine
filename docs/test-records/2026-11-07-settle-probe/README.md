# 안정화 DB 관측의 단일 SELECT 비교

## 범위와 실행

- 실제 격리 PostgreSQL 17.11 primary, 실제 pgx 연결과 SQL을 사용했다.
- 실행 클라이언트는 Linux Docker, Go 1.26.5, x86_64였다. DB와 클라이언트는 로컬 Docker 네트워크다.
- 새 연결과 `pg.Connect`의 Ping을 매 표본에 포함했다. 쿼리 시간과 연결/Ping/Close 포함 시간을 따로 저장한다.
- 기존 세 메서드와 구현된 `pg.DB.SettleStatus`를 조건마다 8회씩 교대 호출했다. 매 두 표본의 순서도 바꾸었다.
- 모든 표본의 recovery/pending restart/server start 값이 같음을 확인했다. 일반적인 primary 상태에서의 관측 비교다.
- 도구: `PG_BENCH_DSN`에 격리 DB URI를 설정한 뒤 `go run ./test/settle-probe-bench -samples 8`.
- DSN·인증정보·데이터 행을 결과에 출력하지 않는다. 원시 결과: [linux-ab.jsonl](linux-ab.jsonl).

## 결과

단위는 ms이며 중앙값이다. `forward delay`는 proxy가 각 방향 TCP read를 전달하기 전에
추가하는 인위적 지연이다. 패킷 분할과 pgx의 처음 prepared statement 준비도 영향을 주므로
이를 특정 네트워크의 정확한 RTT로 바꾸어 해석하지 않는다.

| 각 방향 forward delay | 기존 3 SQL | 단일 SQL | 기존 전체 관측 | 개선 전체 관측 |
| --- | ---: | ---: | ---: | ---: |
| 0 | 3.36 | 2.27 | 10.23 | 9.11 |
| 10 | 127.82 | 43.82 | 217.69 | 133.39 |
| 25 | 308.05 | 104.06 | 517.71 | 313.69 |

원래 쿼리 수 세 개를 한 개로 줄이는 효과이며 전체 migration이나 서버 재시작 단축률은 아니다.
지연 없는 조건의 작은 차이는 이 로컬 호스트의 실행 변동을 포함한다. 측정 시 다른 agent의
독립 kind 환경 준비가 진행될 수 있었으며 CPU 독점 환경은 아니다. 표본 8개의 p95는 상위
표본 하나에 민감하므로 원시 값과 중앙값을 함께 보아야 한다.

## 검사와 제한

- 실제 PG의 단일 관측 결과를 기존 세 메서드와 비교하고 취소한 context에서 오류를 확인했다.
- 엔진 단위 회귀는 optional 경로/legacy 경로, 관측 오류, pending restart, 서버 시작 시각 변경,
  최소 대기와 새 연결 종료를 확인한다. 테스트 fake는 실측 성능의 증거로 사용하지 않았다.
- Windows 클라이언트의 후속 비교가 연결 오류로 일부 중단되어 해당 부분을 완료 결과로 사용하지
  않았다. Linux 비교를 처음부터 다시 실행했고 위 표는 완료된 Linux 결과만 사용한다.
- CNPG health API 왕복, promotion/restart, app cold start, route/release, 실제 클라우드 지연과
  운영 전체 쓰기 보류는 이 DB 관측 벤치의 측정 대상이 아니다.
