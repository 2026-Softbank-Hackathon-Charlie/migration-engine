# 테스트 기록

실행한 테스트를 날짜순으로 남긴다. 실행하지 않은 것과 검증하지 못한 것도 같이 적는다.
비밀값(DSN 비밀번호, 토큰, 키)은 적지 않는다.

| 날짜 | 테스트 | 결과 | 자세히 |
|---|---|---|---|
| 2026-11-04 | platform PG 메이저 버전 게이트 (단위·API·실DB) | 통과 | [아래](#2026-11-04-platform-pg-메이저-버전-게이트) |
| 2026-11-04 | 로컬 kind 이전 리허설 (엔진 왕복·원복) | 4/4 통과 | [아래](#2026-11-04-로컬-kind-이전-리허설) |
| 2026-11-04 | 운영 클러스터 상태 확인 (샘플 앱, 엔진 위치) | 조회만 | [아래](#2026-11-04-운영-클러스터-상태-확인-조회만) |
| 2026-11-04 | 실제 환경 이전 가능 여부 점검 | 플랫폼 경로 불가, CLI 경로 EKS 내부만 가능 | [아래](#2026-11-04-실제-환경-이전-가능-여부-점검-조회만) |
| 2026-11-04 | 로컬 게이트웨이 리허설 (CNPG 1.30.1, gateway http, hello 앱, 부하) | 통과, 원복 직후 쓰기 503 8건 | [아래](#2026-11-04-로컬-게이트웨이-리허설) |
| 2026-11-04 | 엔진 단계별 엣지·실패 케이스 단위 테스트 | 통과, 결함 1건 수정 | [아래](#2026-11-04-엔진-단계별-엣지실패-케이스) |
| 2026-11-04 | 플랫폼 점진 카나리와 실패 시 롤백 e2e (kind) | 3/3 통과 (1회차 observe 시나리오 설계 오류) | [아래](#2026-11-04-점진-카나리와-실패-시-롤백-e2e) |
| 2026-11-04 | platform 버킷 레지던시 측정 (probe·Rego·inspect API, 실 MinIO 2대) | 통과, 결함 1건 수정 | [아래](#2026-11-04-platform-버킷-레지던시-측정) |
| 2026-11-04 | platform 동등성 기록 모드 (RBAC·Prometheus, 실 k3s·Prometheus) | 통과 | [아래](#2026-11-04-platform-동등성-기록-모드) |
| 2026-11-04 | moimroom e2e (부하 중 DB·사진 이전 + 원본 정리, kind 새로 생성) | 통과, 쓰기 대기 15.7초, 실패 요청 0, 전체 7분 10초 | [아래](#2026-11-04-moimroom-e2e-시간-측정) |
| 2026-11-05 | 실제 EKS 이전 리허설 (hello DB, 엔진·부하 클러스터 안에서, 왕복) | 엔진 3/4 성공 (1회차 halt, 원인 port-forward), 응답한 쓰기 누락 0 | [아래](#2026-11-05-실제-eks-이전-리허설) |

---

## 2026-11-04 platform PG 메이저 버전 게이트

- 대상: platform `feat/pg-major-gate` → PR #51 (main `0b218b7`로 머지)
- 실행
  - `go test` internal/preflight/..., internal/plans/..., internal/api/..., internal/runs/..., cmd/... : 통과 (main `cb5e72d` 위로 rebase 후 재실행)
  - `-race` pgversion, plans, api, cmd/api : 통과
  - `go vet ./...`, gofmt, `git diff --check` : 깨끗함
  - 실DB: 로컬 `postgres:17.11-alpine` 컨테이너(127.0.0.1:55439)로 `TestCheckReadsServerVersion` 통과 (서버에서 17 읽음, 같은 메이저 pass, 다른 메이저 fail). 컨테이너는 삭제함
- 기존 실패(이 변경과 무관): `tools/apidoc` TestRenderRepoContract가 로컬에서 실패. 깨끗한 main에서도 같고 CI main은 성공 → 로컬 CRLF 문제로 추정
- 검증하지 않은 것: `image_default`(이미지 미지정)는 막지 않음, 실제 EKS/GKE Plan으로 시작해 보지 않음, 프런트 표시 없음

## 2026-11-04 로컬 kind 이전 리허설

- 대상: migration-engine main `6ef0a64`, `test/rehearsal/run.sh` (`KEEP=1`)
- 환경: Windows 11, Docker 29.6.2, kind v0.27.0 (SHA256 확인), kindest/node v1.32.2, CNPG 1.25.1, kind 클러스터 `hop` 하나에 CNPG 두 개(`pg-src`, `pg-dst`)
- 원본 데이터: orders 20,000행, order_items 15,000행, audit 300행
- 원시 로그: [2026-11-04-local-kind-rehearsal.log](2026-11-04-local-kind-rehearsal.log)

| # | 시나리오 | 기대 | 결과 | 쓰기 다운타임 | 끝난 뒤 |
|---|---|---|---|---|---|
| 1 | pg-src → pg-dst | succeeded | succeeded | 8,771 ms | primary pg-dst, replica pg-src streaming, 쓰기 복제됨, app 로그인 ok |
| 2 | 역방향, demotion 직후 switchover 시간 초과 | rolled_back | rolled_back (15:38:59 → 15:39:27, 약 28초) | – | primary pg-dst 유지, 쓰기 복제됨 |
| 3 | pg-dst → pg-src | succeeded | succeeded | 9,705 ms | primary pg-src, replica pg-dst streaming |
| 4 | pg-src → pg-dst 재이전 (이전 demotion 토큰 남음) | succeeded | succeeded | 20,256 ms | primary pg-dst, replica pg-src streaming |

- 결론: `== all scenarios passed`, exit 0
- 관찰: 4번 다운타임이 1·3번의 두 배 이상. 원인은 조사하지 않음
- 검증하지 않은 것: 클러스터 사이 네트워크, 게이트웨이 전환(`gateway.mode: none`), 앱 트래픽, 파일 이전, 부하
- kind 클러스터 `hop`은 남겨 둠 (`kind delete cluster --name hop`으로 삭제)

## 2026-11-04 운영 클러스터 상태 확인 (조회만)

변경 없음. kubectl get, aws s3api list-buckets, gh api만 실행.

- 워크로드 EKS `charlie-db/postgres-poc`: 1/1, PG 18.6, db_a·db_b 있음, 11/03 백업 완료
- 샘플 앱(moimroom): **배포 안 됨**
  - 관리 EKS ApplicationSet `hopcharlie-apps` 있음(정상), gitops `apps/*/eks.yaml`을 읽지만 gitops에는 `README.md`, `apps/.gitkeep`뿐 → 생성된 Application 0개
  - 워크로드 `charlie-apps` 네임스페이스: 워크로드 없음, Secret 3개(`postgres-poc-ca`, `svc-a-runtime`, `svc-b-runtime`)만
  - 앱 S3 버킷 없음(`hopcharlie-tfstate-...`만), `app-s3` Secret 없음
- 이전 엔진 위치: 별도 pod가 아니라 **platform API 프로세스 안의 라이브러리**(`platform/internal/runs/runs.go`의 `engine.New`)
  - 관리 EKS `charlie-platform`: `hopcharlie-api`, `hopcharlie-gateway`(둘 다 `hopcharlie/platform:0b218b7`), `hopcharlie-console`(`04e6f01`), `hopcharlie-postgres-0` 모두 Running
  - API pod의 `/config/kube`에는 `eks` kubeconfig 하나만 있음 → GKE kubeconfig 없음

## 2026-11-04 실제 환경 이전 가능 여부 점검 (조회만)

변경 없음. infra 레포 원본 읽기, 워크로드 EKS에 `kubectl get`과 `kubectl auth can-i --as`만 실행. 운영 API pod 안의 파일 읽기(`kubectl exec ... cat`)는 권한 분류기가 거부해 하지 않음.

- 플랫폼 환경 정의(infra `terraform/platform/aws/host/envs.yaml`): `gke`(gke-hopcharlie), `eks`(eks-hopcharlie), `onprem`(onprem-hopcharlie)
- 운영 API pod `/config/kube`: `eks` 하나뿐 (`ls`로 확인) → `gke`, `onprem` 컨텍스트 없음
- 워크로드 EKS의 플랫폼 계정 `charlie-system/hopcharlie-platform` (생성 48분 전), 바인딩은 `hopcharlie-platform-observer` 하나

| 권한 (charlie-db 네임스페이스) | 결과 | 엔진에 필요한가 |
|---|---|---|
| list clusters.postgresql.cnpg.io | yes | yes |
| create clusters.postgresql.cnpg.io | **no** | yes (prepare에서 대상 Cluster 생성) |
| patch clusters.postgresql.cnpg.io | **no** | yes (demote/promote) |
| get secrets | **no** | yes (CA·복제 인증서 복사) |
| create secrets | **no** | yes |

- 워크로드 CNPG: operator 1.30.1, plugin-barman-cloud v0.15.1, Cluster는 `charlie-db/postgres-poc` 하나 (1/1 healthy)

판단
- 플랫폼(운영 엔진) 경로 EKS→EKS: **불가**. 엔진 계정에 Cluster·Secret 변경 권한이 없음 (ADR 0036이 `hopcharlie-migrator`를 일부러 주지 않음)
- 플랫폼 경로 AWS↔GCP: **불가**. GKE kubeconfig 없음, 클라우드 사이 DB(5432) 경로 없음 (Tailscale ADR 0037은 TCP 80만, 아직 apply 안 됨)
- 엔진 CLI(로컬 PC, 클러스터 관리자 권한, SSM 터널 + port-forward) 경로 EKS→EKS: **가능해 보임**. 같은 워크로드 클러스터 안의 두 CNPG 사이 이전. 실행은 안 함
- 확인하지 못한 것: 운영 API pod에서 워크로드 DB 5432 도달 여부, 운영 게이트웨이를 이전에 쓸 수 있는지

## 2026-11-04 로컬 게이트웨이 리허설

- 대상: migration-engine `6ef0a64` + 새 `test/gateway-rehearsal/`(미커밋), platform gateway `0b218b7`
- 환경: kind `hopgw`(kindest/node v1.32.2), CNPG **1.30.1**, PostgreSQL **18.6**(operator 기본 이미지). hello 앱 두 벌·platform 게이트웨이·부하기는 호스트 프로세스
- 구성: 부하기 → 게이트웨이(:55580) → `src`=hello-src→pg-src / `dst`=hello-dst→pg-dst. 엔진은 `gateway.mode: http`(admin :55581)
- 부하: `POST /visit` 2 workers × 100ms 간격(약 20 req/s), 2xx id를 `verify.writtenRecords` 형식으로 기록
- 원시 자료(비밀번호 가림): [2026-11-04-gateway-rehearsal/](2026-11-04-gateway-rehearsal/) — run.log, plan-*.out, gateway.log, load-summary.json, failed-requests.jsonl, slow-requests.jsonl

| # | 시나리오 | 기대 | 결과 | 쓰기 보류 시간 (게이트웨이) |
|---|---|---|---|---|
| 1 | 부하 중 pg-src → pg-dst | succeeded | succeeded, 엔진 downtime 13,302 ms | hold 16:09:08.78 → route dst 16:09:22.06 → release 13,291 ms (보류된 쓰기 2건 전달) |
| 2 | pg-dst → pg-src, switchoverTimeout 300ms | rolled_back | rolled_back (16:09:36.2 → 16:10:02.9) | hold 16:09:35.91 → route dst → release 26,951 ms |
| 3 | 쓰기 검사 | 2xx 누락 0 | **누락 0** | – |

쓰기 결과 (load-summary.json)
- 요청 1,163건: 200 1,155건, **503 8건**. 200 중 pg-src 754건, pg-dst 401건
- 최종 primary pg-dst의 visits 1,156행 = 200 응답 1,155건 + 시작 전 확인 요청 1건. 200을 받은 id 중 빠진 것 0건
- 지연: p50 5 ms, p99 420 ms, 최대 26,906 ms(시나리오 2의 보류 중 대기, 게이트웨이 maxHold 55s 이내)

**발견: 원복 완료 5초 뒤 쓰기 503 8건**
- 엔진이 16:10:02.866에 "rolled back; source is primary and serving traffic"을 기록하고 쓰기를 풀었다
- 16:10:08.14~08.49(약 0.35초) 동안 pg-dst에 새 연결이 `the database system is shutting down` → `tls error: EOF` → `the database system is starting up`(SQLSTATE 57P03)으로 실패했다. 게이트웨이도 16:10:08.14에 dst를 unhealthy로 표시
- 해석(추정, 미확인): 다시 promote된 pg-dst를 CNPG가 제자리 재시작한 것으로 보인다(`source.yaml`의 smartShutdownTimeout 주석이 말하는 동작). ADR 0009는 정방향 전환에서 새 primary가 안정될 때까지 쓰기를 붙잡지만, 원복 경로는 이 재시작을 기다리지 않는 것으로 보인다. 엔진 코드로 확인 필요
- 정방향(시나리오 1)에서는 실패 요청 0건

기타 관찰
- 16:09:33 무렵 200 응답이 1.9~2.2초 걸린 요청 2건(시나리오 2 precheck 직전·중). 원인 미조사
- 시나리오 1의 엔진 downtime(13.3s)이 1단계 리허설(8.8s)보다 길다. 부하와 CNPG 버전 차이 중 무엇 때문인지 미조사

검증하지 않은 것: 클러스터 사이 네트워크, 게이트웨이를 클러스터 안에서 돌리는 구성, 플랫폼 API·콘솔 경로, 파일 이전, 여러 번 반복 시 재현성(1회 실행)
kind 클러스터 `hopgw`는 남겨 둠

## 2026-11-04 원복 대기 수정 후 게이트웨이 리허설 재실행

- 수정: `pkg/engine/engine.go` rollback이 원본을 다시 승격한 뒤 `waitSettled`(ADR 0009 보완)를 기다린 다음 route·release (미커밋)
- 단위 테스트: 새 `TestRollbackHoldsWritesUntilSourceSettles`가 수정 전 실패(`src.restart done` 전에 `gw.release`), 수정 후 통과. `go test ./...`, `go test -race ./pkg/engine/`, `go vet ./...`, gofmt(LF 기준), `git diff --check` 통과
- 같은 kind `hopgw`(CNPG 1.30.1, PG 18.6)에서 같은 스크립트 재실행. 원시 자료: [2026-11-04-gateway-rehearsal-after-fix/](2026-11-04-gateway-rehearsal-after-fix/)

| # | 결과 | 비고 |
|---|---|---|
| 1 | succeeded, downtime 13,262 ms | 수정 전 13,302 ms와 같음 |
| 2 | rolled_back | re-promote 16:16:35.99 → source settled 16:17:11.20 (settleMillis 10,624) → route·release 16:17:11.21. 쓰기 보류 약 37.6초 (수정 전 27.0초) |
| 3 | 요청 1,212건 전부 200, **실패 0건**, 2xx 누락 0건 | visits 1,213행 = 1,212 + 시작 확인 1 |

- 지연: p50 5 ms, p99 33 ms, 최대 37,572 ms(시나리오 2 보류 대기)
- 주의: 원복 시 보류가 37.6초로 게이트웨이 maxHold(55초)에 가까워졌다. 원복이 더 길어지면 보류된 쓰기가 `503 hold_timeout`을 받는다. 재승격 뒤 쓰기 가능까지 약 24.6초가 걸린 원인은 조사하지 않음
- 1회 실행 결과이며 재현성은 검증하지 않음

## 2026-11-04 엔진 단계별 엣지·실패 케이스

- 대상: migration-engine `8d8b064`(PR #7 머지 후) + 브랜치 `test/engine-edge-cases`(미커밋)
- 방법: 가짜 CNPG·DB·게이트웨이(`pkg/engine/fakes_test.go`)에 실패 스위치를 더하고(`applyErr`, `healthErr`, `posErrCalls`), 단계마다 실패 지점을 테이블·개별 테스트로 추가
- 추가한 경우와 기대 결과

| 단계 | 경우 | 기대 |
|---|---|---|
| precheck | 원본이 topology primary 아님 / 원본 DB recovery / DB 접속 불가 / 게이트웨이 비정상 / replica.self 충돌 / 대상도 manifest도 없음 | failed, 아무것도 건드리지 않음 |
| prepare | manifest 적용 거부 / 대상이 healthy 안 됨 / 대상이 쓰기 가능(두 번째 primary) | failed, 쓰기 보류 없음 |
| presync | 지연이 안 줄어듦 | failed, 쓰기 보류 없음 |
| presync | 지연 측정 일시 오류 3회 | 재시도 후 succeeded |
| delta | 파일 동기화 실패 | failed, 쓰기 보류 없음 |
| fence | 보류 뒤 fence LSN 읽기 실패 | rolled_back, 원본에 쓰기 해제, 원본 미변경 |
| switchover | 대상이 fence LSN 못 따라옴 | rolled_back, 강등 전 |
| switchover | **강등 요청 자체가 오류(적용 안 됨)** | rolled_back (수정 전: halted) |
| switchover | 승격 요청 오류 | halted (split-brain 방지) |
| switchover | 승격된 대상이 안정되지 않음 | halted, 쓰기 보류 유지 |
| cutover | 경로 변경 뒤 해제 실패 | halted, 트래픽 대상 |
| verify | 옛 원본이 replica로 안 돌아옴 | halted, 트래픽 대상 유지 |
| 원복 | 원본으로 경로 복귀 실패 | halted, 쓰기 해제 안 함 |
| 원복 | 다시 승격한 원본이 안정되지 않음 | halted, 쓰기 보류 유지 |

- 발견·수정한 결함: switchover가 강등 요청 전에 `SourceDemoted`를 기록해서, 요청이 오류로 끝나 실제로는 아무것도 바뀌지 않았는데도 원복이 오지 않을 토큰을 기다리다 halted가 됐다. 원본이 여전히 자기 자신을 primary로 가리키고 새 토큰이 없으면 플래그를 되돌린다(`steps.go`, ADR 0003 보완). 새 테스트는 수정 전 실패(`outcome halted ... source demotion did not finish`), 수정 후 통과
- 원복 전체 시간 여유(`rollbackSlack`, 1분)를 변수로 빼서 테스트에서 줄였다(그 테스트가 63초 → 약 2초)
- 실행: `go test ./...` 통과, `go test -race ./pkg/engine/` 통과, `go vet ./...`, gofmt(LF 기준), `git diff --check` 깨끗함
- 검증하지 않은 것: 가짜 객체 테스트라 실제 CNPG에서 강등 요청이 거부되는 상황은 재현하지 않음. finalize의 실패(대상이 primary가 아님)는 테스트하지 않음

## 2026-11-04 점진 카나리와 실패 시 롤백 e2e

- 대상: platform `0b218b7` + 미커밋 변경(카나리 판단 보류, `CanaryStep.verdict`, e2e 장애 주입), 엔진은 platform go.mod가 고정한 버전
- 환경: platform `test/e2e/run.sh`(kind `hop-e2e`, CNPG 1.25.1, platform API·게이트웨이·테스트 앱 두 벌은 호스트 프로세스, 쓰기 4·읽기 4 사용자 부하)
- 카나리: 1%→10%→25%→50%→75%→90%→100%, 단계마다 5초
- 원시 로그(비밀번호·토큰 가림): [2026-11-04-platform-e2e-failure-scenarios/](2026-11-04-platform-e2e-failure-scenarios/)

| 회차 | 시나리오 | 장애 | 결과 |
|---|---|---|---|
| 1 | canary-fault | 10% 단계 시작 시 대상 읽기 전부 500 | **통과**. 1% inconclusive(요청 0) → 10% failed(오류율 100%) → rolled_back. 게이트웨이 쓰기·읽기 모두 src, 쓰기 5,306건 실패 0, 원본에 전부 있음. 사용자 읽기 실패 0(게이트웨이가 실패한 읽기를 원본으로 재시도한 것으로 보임) |
| 1 | observe-rollback | 전환 후 대상 **읽기만** 1.2초 | **실패(시나리오 설계 오류)**. 대상 요청 대부분이 쓰기라 p95 9ms, 위반 없음. 제품 결함 아님 |
| 2 | observe-rollback | 전환 후 대상 **모든 요청** 1.2초 | **통과**. observe violated(p95 1,439ms > 800ms) → `slo-guard` 롤백 run succeeded(쓰기 다운타임 14.9초). 게이트웨이 쓰기·읽기 src, 쓰기 5,298건 실패 0, 원본에 전부 있음 |
| 2 | migrate | 없음 | **통과**. 1% inconclusive(요청 2) → 10~100% ok, 다운타임 15.1초, 게이트웨이 실패 0, missingWrites 0, 기록한 쓰기 3,048건 전부 새 primary에 있음 |

- 단위 테스트: platform `go test ./...` 중 `tools/apidoc`만 실패(로컬 전용 기존 실패, main에서도 같음). `-race` internal/runs·api 통과. frontend `npm run typecheck`, `npm test` 155/155 통과
- 검증하지 않은 것: 실제 EKS/GKE, 클러스터 사이 네트워크, 정상 이전 후 observe 위반이 없을 때 자동 롤백이 일어나지 않는지(단위 테스트만), CLI 표시(`charlie migrate watch`는 아직 verdict를 모름), 각 시나리오 1회 실행

## 2026-11-04 platform 버킷 레지던시 측정

- 대상: platform `feat/bucket-residency` (기준 main `6028239`), worktree `platform-bucket-residency`
- 실행
  - `go test ./...` (29개 패키지) 통과. `tools/apidoc` TestRenderRepoContract만 실패하는데, 기준 커밋에서도 같이 실패함(이 변경과 무관)
  - `-race` internal/preflight/..., internal/plans 통과. `go vet ./...`, `git diff --check` 깨끗함
  - Rego 변이 확인: 위치, 프리픽스, 복제 규칙을 하나씩 깨면 해당 테스트가 실패함
  - 실 MinIO: 로컬 `pgsty/minio:RELEASE.2026-08-04T00-00-00Z` 2대(A 127.0.0.1:19000 원본, B 127.0.0.1:19001 `MINIO_REGION=kr-onprem-dr`), `-tags minio`
    - bucketprobe: 1단계 프리픽스, 실제 bucket replication(A→B, `arn:minio:replication::…:photos-dr-…`에서 대상 이름 추출), ILM tier 전환 감지, B의 region 읽기, kyc/ 샘플(키·메타데이터 값 없이 `kr_resident_number` 1, `email` 1 개수만)
    - gate 전체(실 probe + 내장 Rego): 선언 망 안 → 허용, 망 밖 CIDR → `bucket_endpoint_outside_network`, 미분류 hosts/ → `unclassified_prefix`, kr-only hosts/를 일본으로 → `prefix_country_not_allowed`
  - 원시 로그(자격 증명 줄 제외): [2026-11-04-bucket-residency-live-minio.log](2026-11-04-bucket-residency-live-minio.log)
- 발견·수정한 결함: region이 설정된 MinIO에 원격 region이 비어 있으면 GetBucketLocation만 통과하고 복제·목록 조회가 서명 region 불일치로 실패함(`bucket_probe:replication_failed`). 저장소가 알려 준 region으로 다시 서명하도록 고침. 가짜 S3 서버로는 재현되지 않았고 실 MinIO에서만 드러남
- 기존 불안정(무관): internal/api `TestRedeployEnvironments`, `TestListAndGetDeployments`가 가끔 실패(환경 순서 gke/eks가 바뀜, #52 코드)
- PR #54 CI 1회차 실패: Formatting 단계(gofmt)에서 `internal/plans/residency_test.go`(import 순서), `internal/preflight/residency/bucket_test.go`(map 정렬)가 걸림. 테스트 실패 아님. 로컬 Windows checkout이 CRLF라 `gofmt -l`이 모든 파일을 보고해 놓쳤음. gofmt로 고친 뒤 LF로 내보낸 트리를 `golang:1.26` 컨테이너(Go 1.26.10)와 `postgres:17.11-alpine`으로 CI 단계 그대로 실행: vet -race, gofmt, 생성 타입 일치, `go test -race ./...`(실패 0, `tools/apidoc` 포함, 로컬 실패는 CRLF 탓으로 확인), `-tags minio` vet·test 통과. 푸시 후 CI go·shell 통과. 그 사이 main에 새 커밋은 없었음
- 검증하지 않은 것: 실제 AWS S3·GCS 버킷(가짜 HTTP 서버로만 확인), 실제 온프렘 Tailscale 주소, rclone 실제 복사와의 연결(rclone 바이너리 없음), AI 라벨 제안(ai-sandbox, 아직 미구현), 콘솔 표시

## 2026-11-04 moimroom e2e 시간 측정

- 대상: platform `test/e2e/moimroom/run.sh` (`KEEP=1`). 다른 작업과 섞이지 않게 세 레포를 `origin/main`으로 따로 복제해서 실행
  - platform `519a969` (API 안의 엔진은 go.mod 고정값 migration-engine `f3b9fd8`, 11/02)
  - migration-engine `c336658` (decommission용 `hopmigrate`만 이 버전으로 빌드)
  - moimroom `eec6253`
- 환경: Windows 11, Docker Desktop(WSL2 12GB), kind v0.33.0, kind 클러스터 `hop-moimroom` 새로 생성, CNPG 1.25.1(운영은 1.30.1)
- 원시 기록: [2026-11-04-moimroom-e2e/](2026-11-04-moimroom-e2e/) (`run.log`는 줄마다 시각을 붙인 전체 출력, `run-state.json`은 엔진 단계별 시각과 증거)

결과

| 항목 | 값 |
|---|---|
| run | succeeded |
| 카나리 10% / 50% | 통과 (72건 p95 18ms / 327건 p95 6ms, 에러 0%) |
| 전환 후 observe 30초 | 3회 모두 통과 (약 1,000건/10초, p95 60~71ms, 에러 0%) |
| 쓰기 대기(downtime, 게이트웨이가 쓰기를 붙잡은 시간) | 15,685 ms |
| 가장 느린 예약 응답 | 15.764초 (쓰기 대기 중에 들어온 요청) |
| 게이트웨이 요청 | 6,445건, 실패 0 |
| 예약 / 사진 / 읽기 | 2,260건 실패 0 / 32장 실패 0 / 4,857건 실패 0 (notFound 141은 카나리 중 대상에 아직 안 넘어온 행·사진, 실패로 세지 않음) |
| verify | missingRows 0, missingWrites 0, missingFiles 0. 기록한 예약 2,260건 모두 새 primary에 있음, 사진 32장 모두 같은 바이트 |
| decommission | PASSED. 옛 앱 중지, 20초 관찰 중 옛 DB 세션 0·원본 요청 0, 차단 확인, 스모크 2/2 |

소요 시간 (run.log 시각 기준)

| 단계 | 시간 | 비고 |
|---|---|---|
| 빌드 (kind, api, gateway, driver, hopmigrate, moimroom 이미지) | 83초 | Go 모듈·이미지 캐시 있는 상태 |
| kind 생성, 이미지 적재, CNPG operator | 72초 | |
| MinIO 2대 | 27초 | |
| 원본 DB 생성 | 77초 | |
| 대상 replica 생성 (이전 전에 미리) | 23초 | |
| moimroom Helm 릴리스 2개 | 13초 | |
| 플랫폼 저장소, 게이트웨이, API | 21초 | |
| **이전 run 전체** | **67.5초** | 아래 상세 |
| decommission | 37초 | 관찰 20초 고정 포함 |
| **전체** | **430초 (7분 10초)** | |

이전 run 67.5초의 내역

| 구간 | 시간 |
|---|---|
| precheck ~ delta (대상이 미리 떠 있어 바로 통과) | 약 1초 |
| 카나리 10% → 50% | 20초 (단계당 10초, 설정값) |
| 전환 승인 | 즉시 (드라이버가 바로 승인) |
| fence (쓰기 붙잡기, 마지막 파일 동기화 0.34초) | 0.4초 |
| switchover: fence gate(해시 비교) | 49 ms |
| switchover: 원본 demote 요청 → 대상 promote 요청 | 약 2초 |
| switchover: promote 요청 → 대상이 쓰기 받을 준비 완료 | 약 13초 |
| cutover (쓰기 풀고 대상으로 보냄) | 0.02초 |
| verify, finalize | 0.7초 |
| 전환 후 observe | 30초 (설정값) |

- 쓰기 대기 15.7초 중 약 13초가 "대상 promote 뒤 쓰기를 받을 준비가 될 때까지"다. 이 구간에 CNPG가 무엇을 하는지는 엔진 이벤트에 기록이 없어 확인하지 못함. 쓰기 대기를 줄이려면 여기부터 봐야 함
- 같은 테스트의 지난 기록(2026-11-02, kind 재사용, README)은 쓰기 대기 15.3초로 비슷함
- 검증하지 않은 것: 클러스터 사이 네트워크(두 환경이 kind 클러스터 하나, 네임스페이스 하나), Tailscale 경로, 운영 CNPG 버전(1.30.1), 최신 엔진 main이 API 안에서 도는 경우(platform이 `f3b9fd8` 고정), 롤백 시나리오(이 테스트는 정방향 한 번만)
- 정리: 로그를 옮긴 뒤 `down.sh`로 이 테스트가 만든 kind 클러스터 `hop-moimroom`, 저장소 컨테이너, 작업 디렉터리를 지움

## 2026-11-04 platform 동등성 기록 모드

- 대상: platform `feat/parity-audit` (기준 main `c846c18`) → PR #55
- 실 검사: k3s `v1.31.4-k3s1` 컨테이너(namespace shop, SA shop-api, configmaps get만 허용) + `prom/prometheus:v2.55.1`(job shop-api up, shop-down down), `cmd/api` TestParityLive로 실제 `parityGate` 연결 경로 사용
  - 기대대로인 대상: identity pass(규칙 3개 authorization_matches), metrics-target scrape_verified, 나머지 probe_not_configured, passed=false
  - 어긋난 대상: identity rbac_mismatch(list-pods fail), metrics target_not_up
  - 원시 로그: [2026-11-04-parity-audit-live.log](2026-11-04-parity-audit-live.log)
- 단위·API: 실패 보고서와 검사 오류가 시작을 막지 않음, 용량 거부·롤백에서는 실행 안 함, 감사 로그 기록, 설정 검증, 30초 상한
- CI 재현: LF로 내보낸 트리를 golang:1.26 + postgres:17.11-alpine 컨테이너에서 vet -race, gofmt, 생성 타입 일치, `go test -race ./...`(31개 패키지, 실패 0), `-tags minio` 통과
- 발견·수정: RBAC 규칙의 `allowed`를 빠뜨리면 조용히 "거부"가 되던 설정 설계를 필수 필드로 바꿈
- 검증하지 않은 것: 실제 EKS/GKE와 운영 Prometheus 주소, 플랫폼 자격 증명의 대상 클러스터 SAR 생성 권한, infra 설정(운영에서는 꺼져 있음)

## 2026-11-05 실제 EKS 이전 리허설

- 대상: migration-engine main `399f278` (`hopmigrate` linux/amd64 빌드), 부하 프로그램 [writer.go](2026-11-05-eks-hello-rehearsal/writer.go)
- 환경: 워크로드 EKS `charlie-dev-workload`, CNPG operator 1.30.1, PostgreSQL 18.6, 네임스페이스 `hello` 하나에 CNPG 두 개(`hello-src`, `hello-dst`, 각 1 인스턴스, gp3 1Gi). 기존 앱과 DB는 건드리지 않음
- 원본 데이터: greetings 50,000행, visits 1,000행(시드)
- 부하: 0.1초마다 `visits`에 한 행. 쓰기를 받는 DB에 쓰고, 실패하면 다른 DB로 옮겨 감(재접속하는 앱 흉내). 응답을 받은 id만 기록하고 끝난 뒤 양쪽 DB에서 모두 있는지 확인
- `gateway.mode: none`. 게이트웨이 쓰기 보류 없이 앱(부하 프로그램)이 직접 새 primary로 옮겨 감
- 입력과 원시 로그: [2026-11-05-eks-hello-rehearsal/](2026-11-05-eks-hello-rehearsal/) (매니페스트, Plan, 실행 파드, 회차별 엔진·부하 로그)

| # | 방향 | 엔진·부하 실행 위치 | 엔진 결과 | 엔진 다운타임 | 부하 결과 |
|---|---|---|---|---|---|
| 1 | src → dst (대상 새로 생성) | 로컬 PC, DB는 `kubectl port-forward` | **halted** at switchover | – | 응답한 쓰기 1,980건 누락 0. 실패 1,515건은 대부분 port-forward 끊김 |
| 2 | dst → src (원복) | 클러스터 안 파드 | succeeded | 8,422 ms | 무효 (부하 프로그램 id 충돌) |
| 3 | src → dst | 클러스터 안 파드 | succeeded | 9,359 ms | 무효 (같은 이유) |
| 4 | dst → src (원복) | 클러스터 안 파드 | succeeded | 13,930 ms | **응답한 쓰기 1,775건 누락 0** (새 primary, 복제본 모두), 실패 25건, 가장 긴 쓰기 공백 2.4초 |

- 4회차 흐름: 07:21:18.2 원본 강등 요청 → 07:21:20.2 대상 승격 요청 → 07:21:20.9 부하가 새 primary에 쓰기 시작 → 07:21:32.0 엔진이 새 primary 안정 확인 후 완료. `hopmigrate verify`: greetings 50,000행, visits 5,395행, 누락 0
- 1회차 prepare(대상 생성과 pg_basebackup) 90초. 이후 회차는 대상이 있어 30ms
- 2→3→4회차는 이전 강등 토큰이 남은 상태에서 연속으로 왕복함. 셋 다 성공
- 엔진 다운타임과 앱 쓰기 공백은 다르다. 엔진은 새 primary가 CNPG 재시작까지 마치고 두 번 연속 정상일 때까지 기다린다(8~14초). 게이트웨이를 쓰는 실제 흐름에서는 쓰기를 이 시간만큼 붙잡는다. 부하 프로그램은 게이트웨이 없이 승격 직후 바로 썼으므로 공백이 2.4초였다

### 1회차 halt

- 엔진이 대상 승격을 요청한 뒤 `target did not settle after promotion: context deadline exceeded`(switchoverTimeout 3분)로 멈췄다. 승격 요청 뒤라 원본을 다시 승격하지 않았다(split-brain 방지, 설계대로). 실제 DB는 dst primary, src replica streaming으로 전환이 끝난 상태였고, 응답한 쓰기는 누락 0
- 원인: DSN이 로컬 `kubectl port-forward`를 거쳤다. 엔진이 돌던 동안 DB 쪽 `connection reset by peer`가 43회 나왔고, 그때마다 port-forward가 통째로 끝나 다시 붙었다. 엔진의 안정 확인(`waitSettled`)은 두 번 연속 성공을 요구하므로 3분 동안 통과하지 못했다. 엔진이 끝난 16:07:54 뒤로 port-forward는 끊기지 않았다
- 확인하지 못한 것: 엔진이 돌던 동안에만 reset이 몰린 이유
- 조치: 2회차부터 엔진과 부하를 클러스터 안 파드(`hello` 네임스페이스에만 권한이 있는 ServiceAccount)에서 실행했다. halted run은 엔진이 재개를 거부하므로(`run ... is halted; start a new run`) 새 run으로 진행했다

### 발견한 것

- 부하 프로그램 결함: 회차마다 id를 1부터 써서 2·3회차에 앞 회차의 id와 충돌했다. 4회차부터 시작 id를 시각 기반으로 바꿨다. 엔진 결과에는 영향이 없다
- `gateway.mode: none`에서도 fence 로그가 `writes held at gateway`라고 찍힌다. 실제로는 아무것도 붙잡지 않으므로 로그 문구가 사실과 다르다. 고치지 않음

### 검증하지 않은 것

- 클러스터 사이 네트워크(EKS ↔ GKE), 게이트웨이 쓰기 보류와 카나리, 플랫폼 API 경로, 파일 이전, 앱 트래픽(HTTP)
- 실패 주입(switchover 시간 초과 → rolled_back)은 이번에 하지 않음

### 남긴 것

- 워크로드 EKS 네임스페이스 `hello`(CNPG 두 개, 실행 파드 `hello-runner`, Role, gp3 1Gi 두 개)는 다음 단계(EKS → GKE)에 다시 쓰려고 남겨 둠. `kubectl delete ns hello`로 정리
