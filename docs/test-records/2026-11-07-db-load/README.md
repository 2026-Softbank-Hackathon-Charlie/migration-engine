# DB 부하 예산 검증

2026-11-07, Docker `golang:1.26.5`, `postgres:17-alpine` 전용 로컬 네트워크에서 실행했다. 운영 DB/클러스터에는 접근하지 않았다.

- `go test -p 2 -tags integration -count=1 -v ./pkg/verify`: 통과. 배치/복합 PK, 부하 승인 실패, 대상 락 시간 초과, 따옴표 포함 키, 비 PK NOT NULL domain, 빈 대상, 기존 UPDATE/DELETE 최종 상태 검사 포함.
- `TestMissingRowsWithSourceFollowingTarget`: 실제 streaming standby 구성이 없어 skip. 독립 DB 간 검증은 실제 PostgreSQL에서 실행했다.
- `go test -p 2 -race -count=1 ./...`: 통과. 부하 초과와 누락/오래된 지표, 정상 완료 취소 경합, write hold 이전 차단, promotion 이후 halt를 포함한다.
- `go vet ./...`, `go build ./...`: 통과.

최종 원시 로그는 `verify-postgres.log`, `race.log`, `vet.log`, `build.log`, `status.log`이다. `initial-*`는 중간 검사 기록이다. `runner.sh`는 읽기 전용 source 및 evidence 마운트를 받아 실행한다. 테스트 DB 접속은 `HOPCHARLIE_IT_PRIMARY_DSN`으로 제공한다.

Prometheus 응답은 단위 검사용 httptest fixture다. 실제 exporter/Prometheus, CNPG 이전, 운영 부하 절감률이나 물리 복제 감속을 검증한 결과가 아니다. 별도 포트의 부하 격리도 주장하지 않는다.
