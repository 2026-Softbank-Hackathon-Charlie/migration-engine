# Issue 19 검증 기록

## 최종 결과 (2026-11-06)

- Windows Go 1.26.5: `go test ./...`, `go vet ./...`, `go build ./...` 모두 exit 0 (`test-final.log`).
- WSL Ubuntu Go 1.26.5, GCC 15.2.0, CGO_ENABLED=1: `go test -race ./...` 전체 통과 (`race-linux.log`).
- 실제 PostgreSQL 17 primary/standby: `go test -tags integration -count=1 -v ./pkg/pg/... ./pkg/verify/...` 전체 통과 (`integration-live.log`). UPDATE/DELETE 최종 상태와 stale 상태 거부 테스트도 PASS이며 SKIP 아님.
- CNPG 1.25.1 / PostgreSQL 17.4 / kind v0.27.0 / Kubernetes v1.32.2: 왕복 리허설 4개 시나리오 모두 통과 (`cnpg-internal.log`).
- `git diff --check`, `bash -n test/rehearsal/run.sh` 통과.

| CNPG 시나리오 | 결과 | 쓰기 전환 구간 |
|---|---|---|
| pg-src → pg-dst | succeeded | 10,111 ms |
| 강등 중 timeout 주입 | rolled_back, 원래 primary 복구·replica streaming 확인 | 로그의 11:32:51~11:33:26 원복 구간 |
| pg-dst → pg-src 정상 복귀 | succeeded | 9,474 ms |
| 과거 demotion token이 남은 상태에서 재이전 | succeeded | 16,926 ms |

첫 이전 후 replica WAL replay를 일시 정지하고, INSERT 100건·UPDATE 100건·DELETE 100건을 한 트랜잭션으로 COMMIT했다. 성공 응답을 받은 뒤 runner에 독립적으로 기록하고 replay를 재개했다. 정상 복귀 후 전체 선택 행의 값과 삭제 상태가 정확히 일치했다. 기존 PK 제약으로 같은 PK의 중복 행은 허용되지 않는다. 외부 부작용이나 요청별 exactly-once 증명은 아니다.

- 독립 기록: `acknowledged.log`, `acknowledged-final-state.txt`
- 복귀 결과: `returned-final-state.txt`
- 두 최종 상태 파일 SHA-256: `108B5C8B49A68E4764F90F3D4D5BE5B435BD779995AB03FB5ACD011B0E3C9283`
- Docker runner `hop-issue19-cnpg`의 die 이벤트 exitCode=0 (`runner-exit.log`). PowerShell은 Docker stderr를 NativeCommandError로 표시했으나 실제 runner는 성공 종료했다.

## 실행 환경과 재현

Docker Desktop을 재기동했다. 기존 `hop-moimroom` 클러스터와 앱 컨테이너는 변경하지 않았다. 환경 버전은 `environment.log`에 있다.

Windows/WSL 호스트에서는 Docker Desktop의 포트 바인딩·게시 포트 메타데이터 문제로 초기 실행이 실패했다 (`cnpg-live.log`). PostgreSQL compose는 `compose-local.yaml`로 호스트 포트를 제거하고, Go 컨테이너를 같은 `hopcharlie-it_default` 네트워크에 연결해 primary:5432 / standby:5432로 실행했다. 이후 compose 리소스는 down -v로 정리했다.

CNPG는 Docker의 kind 네트워크에 붙은 Go runner에서 실행했다. 테스트 사본의 plan만 localhost:55542/55543 대신 hop-control-plane:30432/30433으로 바꾸고, kind export kubeconfig --internal로 API 서버에 접근했다. 실제 실행 wrapper는 `runner.sh`, `kind-wrapper.sh`에 보존했다. `/tools`에는 environment.log 버전의 Linux docker/kubectl/kind-real 바이너리가 필요하다. 원본 plan/클러스터 설정을 운영용으로 바꾸지 않았다. 테스트용 kind-hop kubeconfig는 runner의 임시 경로에만 만들었고 완료 후 hop 클러스터와 runner를 정리했다.

## 이전 실패와 남는 범위

`race.log`는 이전 Windows CGO 비활성화 실패이고 `integration.log`는 이전 DSN 미설정 SKIP이다. 위 Linux race와 실제 PostgreSQL 실행으로 해당 검증 제약을 해소했다. 최초 샌드박스 Go 캐시 접근 거부도 있었으며 재실행 결과를 사용한다.

이슈 #19 전체 완료를 의미하지 않는다. 이 리허설은 한 로컬 kind 클러스터 안의 CNPG 둘이며 실제 클라우드 간 네트워크, gateway 부하, 모든 writer의 통제, primary 저장소/환경 상실, 교차 환경 동기 commit 및 모든 재시작·응답 유실 조합은 검증하지 않았다. 엔진 재시작 경계는 단위 회귀/race 범위로 검증했다. RPO=0 모드는 미지원이다. 운영 인프라에는 배포·장애 주입하지 않았다.
