# migration-engine 상세 문서

HopCharlie의 **결정적 이전 엔진**입니다. 승인된 Plan을 받아 CNPG(PostgreSQL)와 오브젝트 스토리지를 원본 환경에서 대상 환경으로 옮기고, **누락 0건**을 증거로 남깁니다.

AI는 Plan만 만듭니다. 클러스터를 바꾸는 작업은 이 엔진만 합니다.

## 단계

```
precheck → prepare → presync → delta → fence → switchover → cutover → verify → finalize
                                         └──── 쓰기 다운타임 ────┘
```

| 단계 | 하는 일 | 실패하면 |
| --- | --- | --- |
| `precheck` | 원본이 healthy primary인지, 양방향 topology(`replica.self/primary/source`)가 맞는지 확인. rclone과 게이트웨이도 확인 | 종료 (바뀐 것 없음) |
| `prepare` | 대상 Cluster가 없으면 `target.manifest`를 server-side apply. healthy 상태와 replica 여부 확인 | 종료 |
| `presync` | 복제 지연이 `maxLagBytes` 이하가 될 때까지 대기, 파일 1차 `rclone sync` | 종료 |
| `delta` | 파일 변경분 sync, 지연 재확인 | 종료 |
| `fence` | 게이트웨이 **쓰기 보류**, 원본 최종 LSN(fence LSN) 기록, 파일 최종 sync | **자동 원복** |
| `switchover` | 대상이 fence LSN까지 replay했는지 확인 → **fence gate**(테이블별 count와 해시 비교) → 원본 demote → `demotionToken`으로 대상 promote | promote 요청 **전**: 자동 원복 / **후**: 정지 |
| `cutover` | 게이트웨이를 대상으로 전환하고 쓰기 해제. 다운타임 측정 | 정지 |
| `verify` | 원본 PK가 대상에 모두 있는지, k6가 성공한 쓰기가 모두 있는지, `rclone check --one-way` | 정지 (리포트 남김) |
| `finalize` | 양쪽 최종 상태 기록. 옛 원본은 새 primary를 따르는 읽기 전용 replica로 남음 | 정지 |

**원복 경계 (split-brain 방지):** 대상에 promote를 요청하기 전까지만 원본으로 자동 원복합니다. 요청한 뒤에는 `halted` 상태로 멈추고 운영자 판단을 기다립니다.

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

## 다른 영역과의 약속

### A. 인프라 (CNPG 설정)
- 원본과 대상 Cluster 모두 `spec.replica.self / primary / source`를 설정하고, `externalClusters`에 **서로를** 등록해야 합니다 (`examples/cnpg/` 참고). 원본의 `replica.source`는 대상을 가리켜야 합니다.
- 대상이 원본의 `streaming_replica` 인증서와 CA로 접속할 수 있도록 Secret을 양쪽 클러스터에 복사해야 합니다.
- **Argo CD가 `spec.replica`를 되돌리지 않게** 해야 합니다. `ignoreDifferences`에 `/spec/replica`를 넣거나, 이전하는 동안 auto-sync를 끕니다.
- Plan의 `dsn`은 각 클러스터의 `-rw` 서비스로 Tailscale을 통해 닿아야 합니다.
- 대상 Cluster는 **원본의 app Secret을 그대로 써야 합니다** (`bootstrap.pg_basebackup.database/owner/secret`, Secret은 대상 클러스터에 복사). 안 그러면 CNPG가 새 비밀번호를 만들고, 승격하는 순간 app 비밀번호가 바뀌어 앱이 접속하지 못합니다.
- 로컬 kind(CNPG 1.25.1)에서는 WAL 아카이브 없이 스트리밍만으로 demote/promote가 됩니다 (`test/rehearsal/run.sh`). 클러스터 사이 네트워크는 실제 환경에서 다시 확인해야 합니다.

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
