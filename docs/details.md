# migration-engine 상세 문서

HopCharlie의 **결정적 이전 엔진**입니다. 승인된 Plan을 받아 CNPG(PostgreSQL)와 오브젝트 스토리지를 원본 환경에서 대상 환경으로 옮기고, **누락 0건**을 증거로 남깁니다.

AI는 Plan만 만듭니다. 클러스터를 바꾸는 작업은 이 엔진만 합니다.

## 단계

```
precheck → prepare → presync → delta → fence → switchover → start-target → cutover → verify → finalize
                                         └───────── 쓰기 다운타임 ─────────┘
```

| 단계 | 하는 일 | 실패하면 |
| --- | --- | --- |
| `precheck` | 원본이 healthy primary인지, 양방향 topology(`replica.self/primary/source`)가 맞는지 확인. rclone과 게이트웨이도 확인 | 종료 (바뀐 것 없음) |
| `prepare` | 대상 Cluster가 없으면 `target.manifest`를 server-side apply. healthy 상태와 replica 여부 확인. `cold`면 대상 앱을 0개로 줄이고 파드가 없어질 때까지 대기 | 종료 |
| `presync` | 복제 지연이 `maxLagBytes` 이하가 될 때까지 대기, 파일 1차 `rclone sync` | 종료 |
| `delta` | 파일 변경분 sync, 지연 재확인 | 종료 |
| `fence` | 게이트웨이 **쓰기 보류**, 원본 최종 LSN(fence LSN) 기록, 파일 최종 sync | **자동 원복** |
| `switchover` | 대상이 fence LSN까지 replay했는지 확인 → **fence gate**(테이블별 count와 해시 비교) → 원본 demote → `demotionToken`으로 대상 promote | promote 요청 **전**: 자동 원복 / **후**: 정지 |
| `start-target` | `cold`만: 승격된 대상에서 앱을 `replicas`개로 늘리고 ready 대기 → 대상 앱 주소로 조회 스모크. 쓰기 보류가 `maxDowntime`을 넘으면 실패. `warm`(기본)은 건너뜀 | **자동 원복**(대상 강등 → 원본 재승격, 게이트웨이가 쓰기를 붙잡고 있을 때만) |
| `cutover` | 게이트웨이를 대상으로 전환하고 쓰기 해제. 다운타임 측정 | 정지 |
| `verify` | 원본 PK가 대상에 모두 있는지, k6가 성공한 쓰기가 모두 있는지, `rclone check --one-way` | 정지 (리포트 남김) |
| `finalize` | 양쪽 최종 상태 기록. 옛 원본은 새 primary를 따르는 읽기 전용 replica로 남음 | 정지 |

**원복 경계 (split-brain 방지):** 대상에 promote를 요청하기 전까지만 원본으로 자동 원복합니다. 요청한 뒤에는 `halted` 상태로 멈추고 운영자 판단을 기다립니다. 예외는 하나입니다. `cold` Plan의 `start-target`이 실패했고 게이트웨이가 아직 모든 쓰기를 붙잡은 채 원본을 가리킬 때는 사용자 쓰기가 대상에 하나도 가지 않았으므로 승격을 되돌립니다(ADR 0012).

**제로 유실 근거:** CNPG distributed topology에서 대상은 `promotionToken`에 담긴 LSN까지 replay한 뒤에만 승격합니다. 그 전에 엔진이 fence gate로 양쪽 데이터를 한 번 더 비교합니다.

## 사용법

```bash
go build -o bin/hopmigrate ./cmd/hopmigrate

export SRC_PGPASSWORD=... DST_PGPASSWORD=... GATEWAY_TOKEN=...
bin/hopmigrate validate -plan examples/plan.gke-to-eks.json
bin/hopmigrate status   -plan examples/plan.gke-to-eks.json      # 양쪽 CNPG 상태 + 복제 지연
bin/hopmigrate run      -plan examples/plan.gke-to-eks.json      # 전체 실행
bin/hopmigrate run      -plan ... -run <RUN_ID>                  # 중단된 실행 이어가기
bin/hopmigrate step     -plan ... -step precheck                 # 단계별 리허설 (새 run 생성)
bin/hopmigrate step     -plan ... -run <RUN_ID> -step prepare
bin/hopmigrate verify   -plan ...                                # 검증 리포트만 실행
bin/hopmigrate show     -run <RUN_ID>                            # 저장된 상태(증거) 출력
```

`-json`을 붙이면 이벤트가 JSON Lines로 출력됩니다. 플랫폼 API(C)는 이 출력을 그대로 SSE로 넘기거나, `pkg/engine`을 import해서 `Options.Emit`으로 받으면 됩니다.

실행 상태는 `.hopcharlie/runs/<RUN_ID>.json`에 단계마다 저장됩니다. 프로세스가 재시작돼도 `-run`으로 이어서 실행할 수 있습니다.

## 원본 정리 (decommission)

이전 run이 `succeeded`인 뒤에 따로 실행합니다. 이전 결과와 완료 판정은 별개입니다 (ADR 0010).

```
inventory → stop → observe → block → smoke → reconcile
```

| 단계 | 하는 일 | 실패하면 |
| --- | --- | --- |
| `inventory` | 원본 네임스페이스에서 `selector`에 맞는 리소스와 원본 CNPG Cluster·PVC 목록 | 종료 |
| `stop` | 옛 Deployment·StatefulSet을 0으로, CronJob을 suspend. 원래 값은 annotation에 | 원래대로 |
| `observe` | `observe` 동안 옛 DB의 클라이언트 세션(엔진 제외)과 게이트웨이의 원본 backend 요청 수 | 원래대로 |
| `block` | 옛 앱과 옛 DB 파드로 들어오는 접속을 NetworkPolicy로 차단(CNPG 오퍼레이터만 허용). 엔진의 옛 DB 접속이 거부되는지 확인 | 차단 해제, 원래대로 |
| `smoke` | 차단한 채 `smokeUrl`로 `smoke` 요청 | 차단 해제, 원래대로 |
| `reconcile` | 다시 조회해 중지·보존·삭제 승인 대기·미처리로 분류, 리포트 | 차단 해제, 원래대로 |

삭제는 하지 않습니다. 옛 CNPG Cluster와 PVC는 롤백용으로 보존하고, Service·ConfigMap·Secret·Ingress는 "삭제 승인 대기"로 남깁니다.

```bash
bin/hopmigrate decommission -plan plan.json -run <RUN_ID>          # 실행
bin/hopmigrate decommission -plan plan.json -run <RUN_ID> -undo    # 차단 해제, 옛 앱 복구 (반대 방향 이전 전에)
```

Plan에 추가하는 부분:

```json
"decommission": {
  "selector": "app.kubernetes.io/part-of=shop",
  "observe": "2m",
  "smokeUrl": "http://gateway.hopcharlie.ts.net:8000",
  "smoke": [{"method": "GET", "path": "/healthz"}, {"method": "POST", "path": "/orders?k=decommission-smoke"}]
}
```

상태는 `.hopcharlie/runs/<RUN_ID>.decommission.json`에 저장되고, 리포트(`report`)는 화면에 그대로 쓸 수 있게 근거를 나눠 둡니다.

```json
{
  "passed": true,
  "counts": {"unprocessed": 0, "stopped": 2, "retained": 2, "awaitingApproval": 3, "removed": 0},
  "items": [{"kind": "Cluster", "name": "pg-old", "status": "retained", "reason": "old database kept as a replica of the new primary, for migrating back"}],
  "observe": {"windowSeconds": 120, "samples": 61, "maxSourceSessions": 0, "sourceBackendRequests": 0, "notMeasured": []},
  "block": {"policies": ["hopcharlie-decommission-app", "hopcharlie-decommission-db"], "verified": true, "attempts": 4},
  "smoke": [{"method": "GET", "path": "/healthz", "status": 200, "millis": 3, "ok": true}]
}
```

- `maxSourceSessions`, `sourceBackendRequests`가 `null`이면 측정하지 않은 것입니다. 0으로 표시하면 안 됩니다. 이유는 `notMeasured`에 있습니다.
- 관찰 시간 밖에서만 도는 의존성(드문 배치 등)은 잡지 못합니다. 화면에 관찰 시간을 같이 보여 주세요.
- 클러스터의 네트워크 플러그인이 NetworkPolicy를 적용해야 합니다. 적용하지 않으면 `block`이 실패합니다(EKS VPC CNI의 network policy, GKE Dataplane V2).

## 다른 영역과의 약속

### A. 인프라 (CNPG 설정)
- 원본과 대상 Cluster 모두 `spec.replica.self / primary / source`를 설정하고, `externalClusters`에 **서로를** 등록해야 합니다 (`examples/cnpg/` 참고). 원본의 `replica.source`는 대상을 가리켜야 합니다.
- 대상이 원본의 `streaming_replica` 인증서와 CA로 접속할 수 있도록 Secret을 양쪽 클러스터에 복사해야 합니다.
- **Argo CD가 `spec.replica`를 되돌리지 않게** 해야 합니다. `ignoreDifferences`에 `/spec/replica`를 넣거나, 이전하는 동안 auto-sync를 끕니다.
- Plan의 `dsn`은 각 클러스터의 `-rw` 서비스로 Tailscale을 통해 닿아야 합니다.
- 대상 Cluster는 **원본의 app Secret을 그대로 써야 합니다** (`bootstrap.pg_basebackup.database/owner/secret`, Secret은 대상 클러스터에 복사). 안 그러면 CNPG가 새 비밀번호를 만들고, 승격하는 순간 app 비밀번호가 바뀌어 앱이 접속하지 못합니다.
- 양쪽 Cluster에 **`smartShutdownTimeout`을 짧게(예: 5초)** 둡니다. CNPG는 승격 직후 새 primary를 smart shutdown으로 재시작하는데, 기본값(180초)이면 앱 커넥션 풀 때문에 새 primary가 최대 3분 동안 내려가 있습니다. 엔진은 이 재시작이 끝날 때까지 쓰기를 붙잡습니다 (ADR 0009).
- 로컬 kind(CNPG 1.25.1)에서는 WAL 아카이브 없이 스트리밍만으로 demote/promote가 됩니다 (`test/rehearsal/run.sh`). 클러스터 사이 네트워크는 실제 환경에서 다시 확인해야 합니다.

### B. 대상 앱 기동 시점 (`target.app`, ADR 0012)
기동할 때 스키마 마이그레이션을 돌리는 앱(Flyway, Prisma, golang-migrate 등)은 읽기 전용 replica 위에서 뜨지 못합니다. 이런 앱은 `cold`로 두면 대상 DB가 승격된 뒤에 띄웁니다.

```json
"target": {
  "...": "...",
  "app": {
    "mode": "cold",
    "kind": "Deployment", "name": "web", "replicas": 2, "startTimeout": "3m",
    "smokeUrl": "http://web.shop.svc:8080",
    "smoke": [{"method": "GET", "path": "/healthz"}]
  }
}
```

- `mode`: `warm`(기본, 지금 방식: 대상 앱이 전환 전부터 replica 위에서 떠 있음, 카나리 가능) 또는 `cold`
- `cold`는 `gateway.mode: http`가 필요합니다. 승격을 되돌려도 안전한 근거가 "게이트웨이가 모든 쓰기를 붙잡고 있음"이기 때문입니다.
- `smokeUrl`은 게이트웨이를 거치지 않는 대상 앱 주소(예: 대상 클러스터 Service)입니다. 엔진이 그 주소에 닿아야 합니다. `smoke`는 조회(GET, HEAD)만 됩니다. 전환 전에 대상에 쓰지 않기 위해서입니다.
- 쓰기 대기는 settle + 앱 기동 + 스모크 시간만큼 길어집니다. 전환 전 카나리는 쓸 수 없습니다.
- 엔진 계정은 대상 네임스페이스의 Deployment/StatefulSet `get`·`patch`와 Pod `list`가 필요합니다.
- 원본과 대상 앱 버전을 같게 둡니다. 원복하면 대상 앱이 기동 때 남긴 스키마 기록이 원본으로 복제됩니다.

### C. 게이트웨이 HTTP 계약 (`pkg/gateway`, 아직 제안 단계)
| 요청 | 의미 |
| --- | --- |
| `GET /healthz` | 200 |
| `POST /v1/writes/hold` | 새 쓰기를 붙잡고, **처리 중인 쓰기가 끝난 뒤** 200 |
| `POST /v1/writes/release` | 붙잡은 쓰기를 현재 backend로 흘려보낸 뒤 200 |
| `POST /v1/route {"backend":"eks"}` | 트래픽 100%를 해당 backend로 보낸 뒤 200 |

게이트웨이 없이 리허설할 때는 `"gateway": {"mode": "none"}`을 씁니다.

### E. k6 성공 쓰기 기록
k6가 응답 2xx를 받은 쓰기를 한 줄씩 JSONL로 남기면 `verify.writtenRecords`로 대조합니다.
```json
{"table":"orders","pk":"1042"}
{"table":"public.order_items","pk":"(1042,1)"}
```

### 파일 (rclone)
리모트는 환경변수로 설정합니다. 예: `RCLONE_CONFIG_S3_TYPE=s3`, `RCLONE_CONFIG_S3_PROVIDER=AWS`, `RCLONE_CONFIG_MINIO_PROVIDER=Minio`, `RCLONE_CONFIG_MINIO_ENDPOINT=...`

## 알려진 한계
- 게이트웨이를 거치지 않는 쓰기(클러스터 안 배치 작업 등)는 fence가 막지 못합니다. 이전하는 동안 멈춰야 합니다.
- 검증은 PK 집합을 메모리에 올립니다. 시연 규모(수십만 행)에서는 충분하지만, 큰 테이블에는 구간 해시 방식이 필요합니다.
- PK가 없는 테이블은 누락 검사에서 건너뜁니다 (fence gate의 해시 비교에는 포함됩니다).

## 구조
```
cmd/hopmigrate   CLI
pkg/plan         Plan 스키마, 기본값, 검증
pkg/engine       단계 실행기, 상태 저장/재개, 원복 경계, 검증 리포트
pkg/cnpg         CNPG Cluster 조회, apply, demote/promote
pkg/pg           LSN, 복제 지연, recovery 상태
pkg/verify       테이블 해시 비교, 누락 행 검사, k6 기록 대조
pkg/files        rclone sync/check
pkg/gateway      게이트웨이 클라이언트 (HTTP / none)
```
