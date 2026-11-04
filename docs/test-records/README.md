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
