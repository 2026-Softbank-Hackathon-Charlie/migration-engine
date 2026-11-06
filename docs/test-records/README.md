# 테스트 기록

실행한 테스트를 날짜순으로 남긴다. 실행하지 않은 것과 검증하지 못한 것도 같이 적는다.
비밀값(DSN 비밀번호, 토큰, 키)은 적지 않는다.

| 날짜 | 테스트 | 결과 | 자세히 |
|---|---|---|---|
| 2026-11-06 | platform 파일 본문 레지던시 검사·staging 전송 | gofmt·전체 build/vet·LF 전체 test·관련 race·실제 로컬 rclone 통과, fileguard coverage 81.3%. 최초 차단·CRLF apidoc 실패·원본 커밋에서도 재현한 배포 순서 간헐 실패 보존. 운영 이전 미검증 | [기록](2026-11-06-file-content-inspection.md) |
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
| 2026-11-05 | 실제 AWS ↔ GCP 이전 리허설 (EKS ↔ GKE, Tailscale operator 경로, 부하 중) | gateway 없이: EKS→GKE 성공, GKE→EKS rolled_back. **gateway(http)로 왕복 2/2 성공, 실패 요청 0, 쓰기 대기 17.4초, 응답한 쓰기 누락 0** | [아래](#2026-11-05-실제-aws--gcp-이전-리허설) |
| 2026-11-05 | **운영 moimroom 실제 이전 AWS → GCP** (공개 URL 부하 중, 운영 게이트웨이) | 엔진 **halted** at switchover (대상 승격은 성공, port-forward로 안정 확인 실패) → 운영자가 수동 cutover. 응답한 예약 1,487건·사진 70장 누락 0, 쓰기 실패 9건(보류 55초 초과 503), **쓰기 공백 3분 36초** | [아래](#2026-11-05-운영-moimroom-실제-이전-aws--gcp) |
| 2026-11-06 | cold 모드(ADR 0012) 실제 EKS e2e: 기동 때 DDL을 실행하는 앱, 부하 중 | 문제 재현(replica에서 기동 실패) 확인. **정상 전환 성공, 쓰기 대기 12.4초, 실패 0, 누락 0**. **실패 주입 → 승격 되돌림 `rolled_back` 성공**, 누락 0, 쓰기 대기 59.8초(503 1건) | [아래](#2026-11-06-cold-모드-실제-eks-e2e) |
| 2026-11-06 | 쓰기 정지 구간 계측과 smartShutdownTimeout 5초 대 1초 (kind, 부하 중) | 1초에서 정지 13초 → 8.4초, 4회 모두 실패·누락 0 | [아래](#2026-11-06-쓰기-정지-시간-구간별-실측과-단축) |
| 2026-11-06 | 엔진 halt 재개·안정 확인·준비 재시도 단위 테스트, platform lost-response e2e | 통과 (platform 나머지 e2e는 고정된 엔진 버전의 접속 재시도 부재로 1회 실패) | [아래](#2026-11-06-halt-재개-연결-끊김-재시도-멱등) |
| 2026-11-06 | 온프렘 준비: envcheck 실제 실행, 온프렘 manifest 스키마 확인, **클러스터 두 개 사이 이전·역이전** (kind) | 통과. 클러스터 경계를 넘는 복제·Secret 복사·verify-full 확인, 쓰기 정지 8.4초 / 18.9초 | [아래](#2026-11-06-온프렘-준비와-클러스터-간-이전-리허설) |

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

## 2026-11-05 실제 AWS ↔ GCP 이전 리허설

- 대상: migration-engine main `399f278`, 부하 프로그램은 위 EKS 리허설과 같음(시작 id를 시각 기반으로 고친 판)
- 원본: 워크로드 EKS `hello/hello-src` (PG 18.6, CNPG 1.30.1). 위 EKS 리허설 4회차 뒤의 primary
- 대상: GKE `charlie-dev-gcp-workload`의 `hello/hello-gke` (같은 이미지 digest, CNPG 1.30.1, standard-rwo 1Gi). 미리 standby로 만들어 둠(`target.manifest` 생략). 기존 GCP의 `moimroom-gcp-db` 등은 건드리지 않음
- DB 경로: infra [ADR 0048](https://github.com/2026-Softbank-Hackathon-Charlie/infra/blob/main/docs/adr/0048-connect-cloud-databases-with-scoped-migration-access.md) 방식. 양쪽 클러스터에 Tailscale operator 1.104.4와 `hopcharlie-db-network` 차트(hello 값)를 설치. 각 DB는 `db-eks.hopcharlie-db-network.svc.cluster.local`, `db-gke.hopcharlie-db-network.svc.cluster.local` 로컬 Service로 상대에 닿음. 기존 앱 라우터(ADR 0037)는 바꾸지 않음
- 복제 TLS: `verify-full` 유지. 양쪽 CNPG `spec.certificates.serverAltDNSNames`에 상대가 쓰는 프록시 이름을 넣음
- 실행: 엔진과 부하는 EKS `hello` 네임스페이스의 실행 파드. GKE는 `hello` 네임스페이스에만 권한이 있는 ServiceAccount 토큰 kubeconfig로 접근(GKE API는 AWS NAT IP만 허용하며 EKS 파드가 그 IP로 나감)
- `gateway.mode: none`, 부하는 0.1초마다 한 행(a = EKS, b = GKE 프록시)
- 입력과 원시 로그: [2026-11-05-eks-gke-hello-rehearsal/](2026-11-05-eks-gke-hello-rehearsal/) (토큰이 든 kubeconfig는 넣지 않음)

### 준비 중 확인한 것

| 항목 | 결과 |
|---|---|
| GKE 파드 → `db-eks` → EKS hello-src:5432 | 접속됨, 서버 인증서 `CN=hello-src-rw` |
| hello-gke 생성(pg_basebackup over tailnet) | 16:58:06 생성 → 16:59:14 healthy (약 1분, 데이터 작음) |
| hello-gke 복제 | `pg_is_in_recovery = t`, `streaming`, sender `db-eks...` |
| EKS 실행 파드 → `db-gke` → GKE hello-gke:5432 | 접속됨, app 계정 로그인 성공 |
| 엔진 `status` | 양쪽 클러스터 healthy, 복제 지연 0 바이트 |

### 결과

| # | 방향 | 엔진 결과 | 엔진 다운타임 | 부하 |
|---|---|---|---|---|
| X1 | EKS → GKE | **succeeded** | 11,857 ms | 응답한 쓰기 2,329건, GKE·EKS 양쪽 누락 0. 실패 13건, 최장 공백 7.4초 |
| X2 | GKE → EKS (원복 방향) | **rolled_back** (fence gate: `public.visits` 불일치) | – | 응답한 쓰기 2,396건, 양쪽 누락 0. 실패 0, 최장 공백 0.5초 |

- X1 흐름: 08:01:12.1 원본(EKS) 강등 요청 → 08:01:13.3 대상(GKE) 승격 요청 → 08:01:19.9 부하가 GKE에 쓰기 시작 → 08:01:24.8 완료. 끝난 뒤 EKS `hello-src`는 `db-gke`로 GKE를 streaming으로 따라감. `hopmigrate verify` 누락 0
- X2: delta까지 복제 지연 0 바이트였지만, `gateway.mode: none`이라 fence에서 쓰기를 실제로 붙잡지 않는다. fence gate가 양쪽을 비교하던 약 0.5초 동안 부하가 원본(GKE)에 계속 썼고, 클라우드 사이 복제가 따라오기 전에 비교해 불일치가 났다. 엔진은 강등 요청 전에 롤백했고 GKE가 primary로 계속 서비스했다. 설계대로의 안전한 중단이다
- 해석: X1이 같은 경쟁에서 통과한 것은 타이밍이다. 게이트웨이 쓰기 보류 없이는 fence gate 통과가 보장되지 않는다. 실제 흐름(게이트웨이 http)에서 다시 확인해야 한다

### 게이트웨이 경유 왕복 (같은 날, X2 뒤)

X2에서 본 경쟁을 없애려고 쓰기를 HTTP로 플랫폼 게이트웨이(platform main `7568769`, `cmd/gateway`)를 거치게 하고 `gateway.mode: http`로 다시 돌렸다.

- 실행 파드 안에 hello 앱 두 개([hello-app.go](2026-11-05-eks-gke-hello-rehearsal/hello-app.go))와 게이트웨이를 띄움([services.sh](2026-11-05-eks-gke-hello-rehearsal/services.sh)). `app-eks`는 EKS hello-src에, `app-gke`는 `db-gke` 프록시로 GKE hello-gke에 쓴다. **두 앱 프로세스 모두 EKS 파드 안에 있다.** 클라우드가 다른 것은 DB뿐이고, 앱 트래픽이 클라우드를 넘는 경로는 시험하지 않았다
- 부하: 클라이언트 하나가 0.1초마다 `POST /visits?id=N`을 게이트웨이로 보냄(앞 요청이 끝나야 다음 요청). 게이트웨이 기본값(max-hold 55초), 관리 API 인증 끔(파드 loopback)

| # | 방향 | 엔진 결과 | 엔진 다운타임 | 게이트웨이 hold | 부하 |
|---|---|---|---|---|---|
| G1 | GKE → EKS | **succeeded** | 17,398 ms | 17,408 ms, 붙잡은 쓰기 1건 | **실패 0**, 응답한 쓰기 2,229건(GKE 211 → EKS 2,018) 양쪽 누락 0, 가장 느린 쓰기 17.4초 |
| G2 | EKS → GKE | **succeeded** | 17,383 ms | 17,383 ms, 붙잡은 쓰기 1건 | **실패 0**, 응답한 쓰기 2,223건(EKS 209 → GKE 2,014) 양쪽 누락 0, 가장 느린 쓰기 17.9초 |

- 두 회차 모두 fence gate가 통과했다. 쓰기를 붙잡은 뒤 비교하므로 X2와 같은 불일치가 없다
- 흐름(G1): 08:17:17.9 hold → 08:17:18.5 fence gate 통과 → 08:17:18.6 GKE 강등 요청 → 08:17:20.6 EKS 승격 요청 → 08:17:35.3 EKS 안정 확인, release(`backend: eks`). 붙잡힌 쓰기는 EKS로 보내져 201
- 다운타임 17초의 대부분은 승격 뒤 새 primary가 CNPG 재시작을 마치고 엔진이 두 번 연속 정상을 확인할 때까지다(승격 요청 → 완료 약 15초). 같은 클러스터 안(8~14초)보다 길다. 클라우드 사이 경로 때문인지는 나누어 재지 않았다
- `hopmigrate verify` 누락 0. `visits` 14,586행
- 원시 로그: `gw-*-engine.log`, `gw-*-writer.log`, `gw-gateway.log`

### 준비하며 바꾼 것과 발견한 것

- tailnet 정책에 ADR 0048 조각(태그, TCP 5432 grant 3개, accept 테스트 3개)을 병합했다. 기존 `* → *` grant가 있어 deny 테스트는 저장이 거부되므로 넣지 않았다. 그래서 DB 경로는 아직 격리되지 않는다
- OAuth client 2개(`eks`, `gke`)를 API로 만들고 client secret은 각 클러스터 `operator-oauth` Secret으로만 넘겼다. operator 설치(helm)는 Claude Code 권한 분류기가 거부해 사용자가 직접 실행했다
- `hopcharlie-db-network` 차트는 DB 네임스페이스에 primary 파드의 Ingress/Egress를 프록시 5432로만 여는 정책을 만든다. 기존 DNS·API·CNPG 허용 정책이 있다는 전제다. `hello`에는 정책이 없어서 그대로면 DB가 끊기므로 시험용 전부 허용 정책(`hello-allow-all.yaml`)을 먼저 넣었다
- 차트의 egress 프록시는 로컬 CNPG 파드에서 오는 5432만 받는다. 엔진 실행 파드가 상대 DB에 닿도록 시험용 정책(`*-hello-to-proxy.yaml`)을 더했다. 플랫폼 경로에서는 management 프로필이 API 파드를 허용한다
- 원본 CA Secret에는 개인키(`ca.key`)가 들어 있다. 대상은 `ca.crt`만 필요하므로 복사본에서 개인키를 지웠다
- `helm template`은 `--kube-version` 없이 실행하면 차트의 `kubeVersion: >=1.35.0-0 <1.37.0-0` 제약에 걸린다(로컬 helm 기본값 1.37)

### 검증하지 않은 것

- 카나리(읽기 비율), 클라우드를 넘는 앱 HTTP 트래픽(앱은 모두 EKS 파드 안), 플랫폼 API 경로(management 프로필, 임시 RoleBinding helper), 파일 이전, 여러 클라이언트가 동시에 쓰는 부하
- DERP와 직접 연결 중 어느 쪽으로 붙었는지, 큰 DB의 base backup 처리량

### 남긴 것

- EKS `hello`: hello-src(지금 복제본, GKE를 따라감), 실행 파드, Role, 시험용 NetworkPolicy, 복사한 hello-gke CA·복제 Secret
- GKE `hello`: hello-gke(지금 primary), 실행용 ServiceAccount와 토큰, Role, 시험용 NetworkPolicy, 복사한 hello-src CA(인증서만)·복제·앱 Secret
- 양쪽 `hopcharlie-db-network`: operator, db-network 릴리스, 시험용 NetworkPolicy, `operator-oauth`
- tailnet: 정책 병합분, OAuth client 2개, 장치 6개(operator 2, 프록시 4)

## 2026-11-05 운영 moimroom 실제 이전 AWS → GCP

운영 중인 moimroom을 공개 URL(`https://moimroom.hopcharlie.dev`)로 부하를 받는 상태에서 EKS → GKE로 옮겼다. 사용자 승인 아래 실행했다. 입력과 원시 로그: [2026-11-05-moimroom-eks-to-gke/](2026-11-05-moimroom-eks-to-gke/)

- 엔진: migration-engine main `9c10ec7` (Windows 빌드, 로컬 PC에서 실행)
- 원본: 워크로드 EKS `moimroom/moimroom-db` (CNPG 1.30.1, PG 18.6, Argo `moimroom-eks`가 관리, infra #67 ignoreDifferences 적용 확인)
- 대상: GKE `charlie-apps/moimroom-db-gke` 새 standby. 기존 `charlie-apps/moimroom-gcp-db`는 건드리지 않음
- DB 경로: infra ADR 0048 Tailscale operator 프록시(`db-eks`, `db-gke`), `sslmode=verify-full`
- 게이트웨이: 운영 관리 EKS `charlie-platform/hopcharlie-gateway`(`c4cceba`), 앱 `moimroom` 범위(`/apps/moimroom`)
- 파일: S3 `hopcharlie-eks-moimroom` → GCS `charlie-414068132732-moimroom-gcp` (rclone 1.75.2, 시작 전 두 버킷 모두 비어 있음)
- 부하: [load/main.go](2026-11-05-moimroom-eks-to-gke/load/main.go) 를 워크로드 EKS 파드에서 7분. 예약 2명(45~59일 뒤 빈 1시간 칸, 손님 이름 `migration-rehearsal`), 조회 2명, 3초마다 호스트 사진 업로드. 끝난 뒤 업로드한 사진을 모두 다시 받아 SHA-256 비교

### 준비 (이전 전, 트래픽은 EKS 100%)

1. DB 네트워크 릴리스를 hello → moimroom으로 전환하고 hello 리허설 자원 삭제
   - Helm 4 기본 server-side apply가 Tailscale operator 소유 필드(`spec.externalName`)와 충돌해 실패 → `--server-side=false`로 재실행. 실패한 시도는 moimroom 네임스페이스에 허용 정책만 먼저 만들었음
   - **GKE operator DNS 차단**: 차트 정책은 DNS를 kube-dns 파드로만 허용하는데 GKE는 NodeLocal DNSCache(hostNetwork)가 응답한다. operator가 Tailscale API를 못 불러 ingress 프록시를 못 만들었다. operator에만 53 포트를 여는 [gke-operator-dns.yaml](2026-11-05-moimroom-eks-to-gke/gke-operator-dns.yaml) 추가로 해결. 차트 버그로 infra에 수정 필요
   - `charlie-apps`에는 NetworkPolicy가 없어서 차트 정책이 새 DB를 고립시키지 않도록 새 DB 파드에만 [gke-db-baseline.yaml](2026-11-05-moimroom-eks-to-gke/gke-db-baseline.yaml) 적용
2. Secret 복사(값은 출력하지 않음, [secretx.py](2026-11-05-moimroom-eks-to-gke/secretx.py)): EKS → GKE 원본 CA(인증서만)·복제 인증서·app 계정·새 DB 접속 URI·호스트 코드, GKE → EKS 대상 CA(인증서만)·복제 인증서
3. 원본에 인증서 SAN → 대상 생성 → 원본에 `replica`·`externalClusters` 순서로 패치. 원본은 재시작 없음, 공개 healthz 200 유지. Argo `moimroom-eks`는 Synced 유지(ignoreDifferences 동작 확인)
4. 대상 생성 후 pg_basebackup 약 20초, `streaming` lag 0
5. GKE 앱 `moimroom-gcp`의 `DATABASE_URL`을 새 DB로, `SEED_SPACES=false`로 바꾸고 1대 기동(standby라 스키마 마이그레이션 건너뜀 로그 확인). 게이트웨이 `moimroom`에 `gke` 백엔드(`http://10.80.0.7:80`) 추가, 쓰기·읽기는 EKS 유지. 두 백엔드 healthy

### 실행 (KST)

| 시각 | 사건 |
|---|---|
| 22:14:08 | 부하 시작 |
| 22:14:37 ~ 22:14:41 | precheck ~ delta (lag 0, 파일 presync·delta) |
| 22:14:41.1 | fence: 게이트웨이 쓰기 보류 |
| 22:14:42.1 | fence gate 통과 (테이블 5) |
| 22:14:42.2 | 원본 강등 요청 |
| 22:14:43.4 | 대상 승격 요청 |
| 22:14:55 | **GKE DB 승격 완료** (Postgres 로그: `server promoted`, 재시작 없음) |
| 22:15:36, 22:16:31, 22:17:26 | 보류 55초 초과로 쓰기 503 `hold_timeout` (회당 예약 2, 사진 1) |
| 22:17:41.8 | **엔진 halted**: `target did not settle after promotion` (switchoverTimeout 3분). 승격 요청 뒤라 자동 롤백 안 함 |
| 22:18:17 | 운영자가 DB 상태(GKE primary, EKS replica) 확인 후 게이트웨이 `route gke` + `writes/release` 수동 실행 |
| 22:21 | 부하 종료 |

### 결과

| 항목 | 값 |
|---|---|
| 예약 성공 / 실패 / 충돌 | 1,487 / 6 / 0 |
| 사진 업로드 성공 / 실패 | 70 / 3 |
| 조회 성공 / 404 / 실패 | 5,597 / 0 / 0 (eks 3,886, gke 1,711) |
| 쓰기 실패 원인 | 9건 모두 게이트웨이 `503 hold_timeout` (커밋 안 됨) |
| 가장 긴 예약 공백 | **3분 36.6초** (22:14:41.2부터) |
| 응답한 예약이 GKE DB에 있음 | 1,487 / 1,487 (누락 0), DB의 `migration-rehearsal` 행도 정확히 1,487 |
| 응답한 예약이 EKS(이제 replica)에 있음 | 1,487 / 1,487 |
| 사진 재확인 (GKE 경로) | 70 / 70 같은 바이트(SHA-256), 누락 0 |
| `hopmigrate verify` (따로 실행) | 테이블 5개 누락 0, 파일 10 일치, `RESULT: OK` |
| 끝난 뒤 | GKE `moimroom-db-gke` primary, EKS `moimroom-db` replica streaming lag 0, 게이트웨이 쓰기·읽기 gke 100% |

### halt 원인 (일부만 확인)

- 대상 DB는 22:14:55에 승격을 마치고 연결을 받았다(재시작 없음). 엔진만 3분 동안 안정을 확인하지 못했다
- 엔진의 대상 DSN은 로컬 `kubectl port-forward`를 거쳤다. 승격 직후부터 엔진이 끝날 때까지 port-forward가 93번 끊겼다(파드 안 `127.0.0.1:5432` 연결이 `connection reset by peer`). 엔진이 끝난 22:17:41 뒤로는 끊기지 않고 지금도 정상
- 같은 증상이 hello 1회차(port-forward 경로)에서도 나왔다. 클러스터 안에서 엔진을 돌린 회차는 모두 성공했다
- 확인하지 못한 것: 엔진이 연결하는 동안에만 reset이 반복되는 이유
- 엔진을 PC에서 돌린 이유: 워크로드 EKS 파드에서 관리 EKS 게이트웨이 관리 포트(9901)로 가는 길을 보안 그룹이 막는다(관리 노드는 ALB 보안 그룹에서만 80~9901 허용). 사용자가 보안 그룹 임시 변경 대신 PC 실행을 골랐다

### 쓰기 공백 3분 36초

- 게이트웨이는 엔진의 cutover를 기다리며 보류를 유지했다. 엔진이 halt한 뒤에도 보류는 풀리지 않았고, 운영자가 수동으로 풀 때까지 이어졌다. 보류 중 들어온 쓰기는 55초마다 503으로 끝났다
- 엔진이 정상 동작했다면 승격 완료(22:14:55) 뒤 안정 확인만큼 기다리고 cutover했을 것이다. hello 게이트웨이 회차의 보류는 17.4초였다
- halt가 쓰기 보류를 3분 넘게 유지한 것은 설계상 맞다(승격 요청 뒤에는 어느 쪽이 primary인지 사람이 확인해야 한다). 다만 halt 때 운영자에게 알리는 수단이 엔진 로그뿐이었다

### 검증하지 않은 것과 남은 것

- 엔진 run 상태는 `halted`로 남아 있다. cutover·verify·finalize는 엔진이 하지 않았고, cutover는 운영자가 게이트웨이 API로, verify는 따로 실행했다
- 롤백(GCP → AWS)과 실패 주입은 하지 않았다
- GitOps: EKS 앱과 DB는 Argo가 계속 관리한다(지금 EKS DB는 replica, EKS 앱은 트래픽 없음). GKE 앱 변경(`DATABASE_URL`, `SEED_SPACES`, replicas 1)은 kubectl patch로 했고 Git에 없다. infra `apps.yaml`의 gke 대상도 아직 `moimroom-gcp-db`
- 부하는 AWS 리전 파드에서 공개 URL로 보냈다. 실제 사용자 위치와 다르다
- 남긴 것: 워크로드 EKS `hop-migrate`(러너 파드, env Secret, 프록시 허용 정책), `moimroom/*-gke-*` Secret 2개, GKE `charlie-apps`의 러너 SA·토큰·Role, 복사한 Secret 5개, `moimroom-db-gke-baseline`, `hopcharlie-db-network/db-network-operator-node-local-dns`. 테스트 예약 1,487건과 사진 70장(손님 이름 `migration-rehearsal`)은 운영 DB와 GCS에 남아 있음

## 2026-11-06 cold 모드 실제 EKS e2e

ADR 0012(대상 앱을 승격 뒤에 띄우는 cold 모드)를 실제 클러스터에서 확인했다. 입력과 원시 로그: [2026-11-06-cold-start-target-eks/](2026-11-06-cold-start-target-eks/)

- 엔진: `feat/cold-start-target` `79a3513`(PR #13) linux 빌드, 게이트웨이: platform main `fb3eb20`
- 환경: 워크로드 EKS, 네임스페이스 `cold-test` 하나에 CNPG 1.30.1 두 개(`cold-src`, `cold-dst`, PG 18.6, gp3 1Gi, `smartShutdownTimeout: 5`). 엔진·게이트웨이·부하는 같은 네임스페이스의 러너 파드(이 네임스페이스 권한만, Secret 권한 없음)에서 실행. 운영 앱은 건드리지 않음
- 앱: [app/main.go](2026-11-06-cold-start-target-eks/app/main.go). 시작할 때 goose v3.26.0 `Up`을 돌린 뒤, 매번 `CREATE TABLE IF NOT EXISTS`를 실행한다(끄는 옵션 없음). Deployment 2개(`app-src`, `app-dst`, 각 2대), readiness `/healthz`
- 부하: 게이트웨이를 거쳐 0.1초마다 `POST /visits` 한 건(순차 클라이언트 하나), 4분. 응답 201을 받은 id만 기록하고 끝난 뒤 양쪽 DB에서 확인

### 1. 문제 재현: replica 위의 대상 앱

| 이미지 | replica 위에서 |
|---|---|
| v1: goose `Up`만 | **정상 기동**. 적용할 마이그레이션이 없으면 goose는 아무것도 쓰지 않는다(`no migrations to run`) |
| v2: goose `Up` + 매번 `CREATE TABLE IF NOT EXISTS` | **기동 실패**, CrashLoopBackOff: `cannot execute CREATE TABLE in a read-only transaction (SQLSTATE 25006)` |

goose를 쓰는 앱 자체는 warm 모드로 옮길 수 있다. 시작할 때 무엇이든 쓰는 앱(이 v2, 고치기 전 moimroom)이 cold 모드 대상이다.

### 2. cold 정상 전환 (src → dst)

| 시각 (UTC) | 사건 |
|---|---|
| 00:18:26.0 ~ 27.2 | precheck, prepare(대상 앱 0개로, 파드 없어질 때까지 1.1초) |
| 00:18:27.25 | fence: 쓰기 보류 |
| 00:18:27.3 | fence gate 통과(테이블 3) |
| 00:18:29.4 | 대상 승격 요청 |
| 00:18:36.6 | switchover 완료(settle 포함 9.3초) |
| 00:18:36.6 → 39.6 | **start-target 3.1초**: 2대로 늘림 → 2대 ready → 스모크 2건 통과 |
| 00:18:39.64 | cutover, 쓰기 대기 **12,393 ms** |

- 부하: 2,279건 모두 201(src 213, dst 2,066), **실패 0**, 가장 긴 쓰기 공백 12.43초
- 응답한 2,279건이 새 primary(dst)와 replica(src)에 모두 있음(누락 0)
- 대상 앱은 승격된 primary 위에서 같은 DDL을 통과하고 기동(로그 `dst listening`). prepare가 남긴 Stop annotation은 Scale이 지움

### 3. 실패 주입 → 승격 되돌림 (dst → src)

대상 앱 `app-src`에 존재하지 않는 DB 호스트를 배포해 승격 뒤에도 뜨지 못하게 했다. `startTimeout` 40초.

| 시각 (UTC) | 사건 |
|---|---|
| 00:23:03.67 | fence: 쓰기 보류 |
| 00:23:05.8 | 대상(src) 승격 요청 |
| 00:23:12.96 | switchover 완료(9.3초) |
| 00:23:12.98 | 대상 앱 2대로 늘림 |
| 00:23:52.97 | start-target 실패: 40초 동안 ready 0/2 |
| 00:23:54.02 | 원복: 대상 앱 정지(1.1초) |
| 00:23:54.09 | 원복: 대상(src) 강등 요청 |
| 00:23:56.18 | 원복: 원본(dst)을 대상의 강등 토큰으로 재승격(강등 토큰까지 2.1초) |
| 00:24:03.42 | 원복: 원본 settle(6.2초), 복제 지연 0 확인 |
| 00:24:03.44 | 원본으로 route·release, **`rolled_back`** (원복 자체 10.5초) |

- 결과: dst primary, src replica(streaming), `app-src` 0대, `app-dst` 2대 그대로 서비스. 상태 파일의 플래그(`writesHeld`, `sourceDemoted`, `promoteRequested`, `trafficOnTarget`) 모두 false
- 부하: 1,805건 201, 실패 1건(503, 게이트웨이 보류 55초 초과), 가장 긴 쓰기 공백 59.8초
- 응답한 1,805건이 dst·src 모두에 있음(누락 0). 두 DB의 visits 전체 행 4,084 = 2회차 2,279 + 3회차 1,805. 실패한 1건은 커밋되지 않았고, 원복 과정에서 남거나 빠진 행이 없다

### 발견한 것

- **쓰기 대기가 게이트웨이 최대 보류(55초)를 넘으면 사용자 쓰기가 503으로 끝난다.** 원복 회차는 settle 9.3초 + `startTimeout` 40초 + 원복 10.5초로 59.8초를 붙잡았다. 엔진은 게이트웨이의 최대 보류 값을 모른다. cold Plan의 `startTimeout`은 "게이트웨이 최대 보류 − settle − 원복 시간"보다 짧게 잡아야 한다. 엔진이 이 값을 받아 검증하게 할지는 후속 과제다
- goose v3.26.0은 적용할 마이그레이션이 없으면 replica에서도 뜬다(위 1)

### 검증하지 않은 것

- 클러스터 사이(EKS ↔ GKE) cold 전환. 이번은 한 클러스터 한 네임스페이스
- platform API 경로(카나리 거부, 콘솔 표시)는 아직 구현 안 함
- StatefulSet 대상 앱, 실제 오픈소스 앱(Flyway·Liquibase 사용 앱 등)

### 정리

- 네임스페이스 `cold-test`(DB 2개와 PVC, 앱, 러너, RBAC) 삭제, ECR `hopcharlie/platform`의 `cold-test-goose-1`, `cold-test-goose-2` 태그 삭제

## 2026-11-06 쓰기 정지 시간 구간별 실측과 단축

- 대상: migration-engine 브랜치 `feat/shorter-safer-write-pause`(미커밋, main `292ebaf` 기반), platform 게이트웨이 `fb3eb20`
- 환경: 로컬 kind `hopgw`, CNPG 1.30.1, PG 18.6, 인스턴스 1, `test/gateway-rehearsal/run.sh`(hello 앱 두 벌, 부하 약 20 req/s), 다른 부하 없음
- 원시 자료(비밀번호 가림): [2026-11-06-write-pause/](2026-11-06-write-pause/)

정지 구간 계측(엔진 증거 `timingMillis`, `settle`), smartShutdownTimeout 5초:

| 구간 | 시간 |
|---|---|
| fence LSN 따라잡기 + fence gate | 0.03초 |
| 원본 강등 | 1.0초 |
| 승격 → 쓰기 가능 | 1.0초 |
| 안정 대기 | 11.0초 (pending_restart 1, 재시작 1, 접속 실패 11, CNPG 비정상 9) |

- 원인(PostgreSQL·CNPG 로그로 확인): 승격 때 `archive_mode`가 바뀌어(`cannot be changed without restarting the server`) CNPG가 0.2초 뒤 제자리 재시작을 결정하고 smart shutdown을 요청한다. smart shutdown이 smartShutdownTimeout(5초)을 다 기다린 뒤 fast로 넘어간다

smartShutdownTimeout 5초 대 1초(번갈아 2회씩):

| 회 | 설정 | 정상 이전 쓰기 정지 | 안정 대기 | 원복 시나리오 보류 | 실패 요청 | 누락 |
|---|---|---|---|---|---|---|
| 1 | 5초 | 13.4초 | 11.3초 | 37.8초 | 0 / 1,211 | 0 |
| 2 | 1초 | **8.4초** | 6.2초 | 32.6초 | 0 / 1,247 | 0 |
| 3 | 5초 | 12.9초 | 10.8초 | 37.8초 | 0 / 1,174 | 0 |
| 4 | 1초 | **8.4초** | 6.3초 | 33.2초 | 0 / 1,213 | 0 |

- 결론: 1초로 쓰기 정지 약 4.8초(35%) 단축. examples, 리허설 manifest, `docs/details.md` 권장값을 1초로 바꿈(ADR 0013)
- 버린 측정: 다른 kind 클러스터(platform e2e)가 동시에 돈 1초 실행은 19.3초(강등·승격이 5~6초로 늘어남). CPU 경합이라 결론에 쓰지 않음
- 검증하지 않은 것: 실제 EKS/GKE에서의 단축 폭, 인스턴스 2개 이상 클러스터, 1초가 앱 커넥션 풀에 주는 영향(재시작 때 세션이 fast shutdown으로 끊김. 이번 부하에서는 실패 0)

## 2026-11-06 halt 재개, 연결 끊김, 재시도, 멱등

엔진 단위 테스트(가짜 객체, 브랜치 `feat/shorter-safer-write-pause`):

| 테스트 | 내용 | 결과 |
|---|---|---|
| `TestSettleToleratesDroppedConnections` | 승격부터 cutover까지 접속이 하나 걸러 실패(11/05 운영 port-forward 증상) | 통과. 바꾸기 전 안정 확인으로 돌리면 `halted at switchover: target did not settle` 로 실패하는 것을 확인 |
| `TestSettleSeesASilentRestart` | 다른 신호 없이 서버 시작 시각만 바뀜 | 재시작 1회로 셈 |
| `TestResumeHaltedAfterTargetSettles` | 안정 대기 시간 초과로 halted → 대상 안정 후 `ResumeHalted` | cutover·verify·finalize까지 succeeded, 승격 요청은 한 번뿐 |
| `TestResumeHaltedRefusesWithoutProof` | 승격 전 halt / 대상이 primary 아님 / 원본이 쓰기 가능 | 세 경우 모두 재개 거부, run 그대로 |
| `TestSlowFenceGateFailsBeforeTheHold` | fence gate 리허설이 maxFenceGate를 넘음 | delta에서 failed, 쓰기 보류 없음 |
| `TestPrepareRetriesTargetConnection` | 새 대상의 첫 접속 3번 실패 | 재시도 후 succeeded |

- `go test ./...`, `go vet ./...` 통과

platform e2e(worktree `.wt-platform-retry`, 브랜치 `feat/retry-safe-writes`, 미커밋, kind `hop-e2e`, CNPG 1.25.1):

| 회 | 시나리오 | 결과 |
|---|---|---|
| 1 | lost-response | **통과**. 원본 앱이 5번째 쓰기마다 커밋 뒤 응답 없이 끊음 → 첫 시도 실패 151건 모두 같은 id 재시도로 완료, 실패 0, 행 895 = 응답한 쓰기 895(중복 0, 모르는 행 0) |
| 1 | canary-fault | 실패: platform main의 새 규칙(AI가 낸 Plan은 리허설 보고서 필요, platform #24)으로 승인 409. 드라이버가 Plan을 operator로 내도록 고침 |
| 2 | lost-response | **통과**. 첫 시도 실패 154건 모두 재시도로 완료, 실패 0, 행 908 = 응답 908 |
| 2 | canary-fault | 실패: prepare에서 대상 DB 첫 접속 `tls error: EOF` 한 번으로 run failed. platform이 고정한 엔진에는 접속 재시도가 없음 → 엔진에 재시도 추가(`TestPrepareRetriesTargetConnection`) |

- 검증하지 않은 것: 엔진 수정이 들어간 platform으로 나머지 e2e(canary-fault, observe-rollback, migrate) 재실행은 platform이 엔진을 올린 뒤. 실제 클라우드에서 `-resume-halted` 사용

## 2026-11-06 온프렘 준비와 클러스터 간 이전 리허설

- 대상: migration-engine 브랜치 `feat/onprem-readiness`(main `5913052` 기반, 미커밋 변경 포함) — `hopmigrate envcheck`(ADR 0014), `examples/cnpg/target-onprem.yaml`, `docs/onprem.md`, `test/cross-cluster-rehearsal`
- 원시 자료(비밀번호 가림): [2026-11-06-cross-cluster/](2026-11-06-cross-cluster/)

envcheck 실제 실행(kind `hopgw`, CNPG 1.30.1):
- 정상: kube-api, namespace, storage-class(기본 `standard`, local-path), cnpg-crd(pg-src·pg-dst 이미지 표시), cnpg-operator 1.30.1 일치, 권한 8개 allowed, postgres(18.6 primary) 모두 pass, exit 0
- 일부러 틀린 입력: 없는 네임스페이스 fail, operator 버전 1.29.0 기대 → warn, `-pg-major 17` → fail(18과 다름), 닿지 않는 MinIO → fail, exit 1
- 단위 테스트(가짜 클라이언트): 준비된 k3s(`v1.33.5+k3s1`), CNPG 없음·권한 거부·DB 접속 실패(비밀번호가 보고서에 안 남음), StorageClass 없음, API 불통(나머지 중단)

온프렘 manifest: `examples/cnpg/target-onprem.yaml`을 같은 kind의 CNPG 1.30.1에 server dry-run → 스키마 통과. `examples/plan.eks-to-onprem.json` → `hopmigrate validate` 통과

클러스터 간 이전 리허설(`test/cross-cluster-rehearsal/run.sh`):
- 환경: kind 두 개 `hopcloud`(원본), `hoponprem`(대상), 둘 다 CNPG 1.30.1, PG 18.6, 인스턴스 1, smartShutdownTimeout 1. 서로의 DB는 docker 네트워크의 노드 이름과 NodePort로, `sslmode=verify-full`(certificates.serverAltDNSNames)
- 순서(11/05 운영 EKS → GKE와 같음): 원본 생성 → 원본 CA(인증서만)·복제 인증서·app Secret을 대상 클러스터로 복사 → 대상 생성(클러스터 간 pg_basebackup, 72초 뒤 healthy·streaming) → 대상 CA·복제 인증서를 원본 클러스터로 복사 → 원본 replica 토폴로지 패치 → 엔진

| # | 시나리오 | 결과 | 쓰기 정지 | 끝난 뒤 |
|---|---|---|---|---|
| 1 | cloud → on-prem | succeeded | 8.4초 (승격→쓰기 가능 1.0초, 안정 6.3초) | primary pg-dst(onprem), pg-src replica `streaming from hoponprem-control-plane`, 쓰기가 클러스터를 넘어 복제됨 |
| 2 | on-prem → cloud | succeeded | **18.9초** (승격→쓰기 가능 **11.6초**, 안정 6.3초) | primary pg-src(cloud), pg-dst replica `streaming from hopcloud-control-plane`, 쓰기 복제됨 |

- 관찰: 2번의 승격 → 쓰기 가능이 1번보다 10초 길었다. 원인은 조사하지 않음(18초 전에 강등된 클러스터를 바로 다시 승격한 영향인지 확인 필요)
- 검증하지 않은 것: 실제 k3s(이번은 kind 두 개), Tailscale 경로, 게이트웨이와 부하(gateway none), 파일 이전, 다른 네트워크 지연이 있는 실제 온프렘
