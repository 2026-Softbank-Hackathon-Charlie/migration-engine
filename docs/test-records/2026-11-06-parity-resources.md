# 2026-11-06 platform 동등성 부분 증거 프로브

## 대상과 환경

- 저장소: platform, `feat/parity-resource-probes`. 최초 base `80e79d5`/commit `efc0a1d`, 작업 중 PR #79가 병합되어 최종 base `d968e77`, 최종 commit `03802b1`로 rebase.
- 환경: Windows 11, Go 1.26.7 windows/amd64, 로컬 client-go, httptest 합성 Kubernetes 응답. 실 EKS/GKE/k3s가 아니다. LF 검증은 같은 Windows 도구로 `core.autocrlf=false` 임시 worktree에서 실행했다(Linux 실행 아님).
- 범위: Secret metadata 전용 협상, 감사 수집기 Deployment 상태, CNPG/Barman 백업 상태, audit 전용 gate 연결과 환경별 설정.
- 운영 배포·클러스터 변경·IAM/RBAC 변경 없음.

## 실행 기록

| 검사 | 결과 | 원시 로그 |
|---|---|---|
| 최초 대상 패키지 테스트 | 실패 1건: 정상 PartialObjectMetadata의 TypeMeta를 client-go가 비워 probe에서 unknown으로 처리함 | [targeted](2026-11-06-parity-resources-targeted.log) |
| metadata 진단 테스트 | 같은 실패 재현. 반환 타입은 PartialObjectMetadata지만 apiVersion/kind가 비어 있음 확인 | [metadata-debug](2026-11-06-parity-resources-metadata-debug.log) |
| 수정 후 대상 패키지 `go test -count=1` (probes, paritygate, envs, cmd/api) | 통과. type 검증은 디코딩 전 metadata 전용 transport에서 수행하고 일반 Secret 응답 거부 | [targeted-retry](2026-11-06-parity-resources-targeted-retry.log) |
| `go test -count=1 ./...` (최신 main base 반영) | 새 검사 포함 런타임 패키지 통과. `tools/apidoc/TestRenderRepoContract` 실패: `chunk 1 is 151955 bytes, over 20000`. 동일 변경 LF checkout에서는 전체 통과(아래). Windows 작업 트리 줄바꿈과 관련된 실패로 보이며, apidoc 코드는 변경하지 않았다 | [full](2026-11-06-parity-resources-full.log) |
| `go vet ./...` | 통과 (exit 0, 출력 없음) | [vet](2026-11-06-parity-resources-vet.log) |
| 변경 패키지 `go test -race -count=1` (probes, paritygate, envs, cmd/api) | 통과 | [race](2026-11-06-parity-resources-race.log) |
| 최종 변경 패키지 + plans/api `go test -race -count=1` | 통과. 추가한 timeout/backup target mismatch 테스트 포함 | [final-race](2026-11-06-parity-resources-final-race.log) |
| 최초 commit LF worktree `go test -count=1 ./...` | 전체 통과. apidoc 포함 | [full-lf](2026-11-06-parity-resources-full-lf.log) |
| 최초 commit LF `go vet -race ./...` / `gofmt -l .` | 통과, 출력 없음 | [vet-race-lf](2026-11-06-parity-resources-vet-race-lf.log), [format-lf](2026-11-06-parity-resources-format-lf.log) |
| 최초 commit LF 전체 race | `TestRedeployEnvironments` 1건 실패. 재배포 대상이 기대한 gke만이 아니라 gke+eks로 반환됨 | [full-race-lf](2026-11-06-parity-resources-full-race-lf.log) |
| 변경 없는 main `d968e77`에서 `go test -race -count=30 ./internal/api -run '^TestRedeployEnvironments$'` | 같은 실패 4건 재현. 이번 parity 코드 없는 main에도 존재. 원인은 이번 범위에서 수정하지 않음 | [main-redeploy](2026-11-06-parity-resources-main-redeploy.log) |
| **최종 rebase commit `03802b1` LF 전체 `go test -count=1 ./...`** | **통과** | [full-lf-rebased](2026-11-06-parity-resources-full-lf-rebased.log) |
| **최종 rebase commit LF 전체 `go test -race -count=1 ./...`** | **통과**. 앞의 main 재배포 테스트 간헐 실패가 해소됐다는 의미는 아님 | [full-race-lf-rebased](2026-11-06-parity-resources-full-race-lf-rebased.log) |
| **최종 rebase commit LF `go vet -race ./...`** | **통과**, 출력 없음 | [vet-race-lf-rebased](2026-11-06-parity-resources-vet-race-lf-rebased.log) |
| `git diff --check` / staged whitespace 검사 | 통과 | 명령 exit 0 확인 (git의 CRLF 변환 안내만 있음) |

## 확인한 계약

- Secret: 존재/404/401/403/406, metadata 전용 Accept header, full Secret 응답 거부, 원문·annotation·실제 이름 비노출, 잘못된 target 거부.
- 감사: 현재 generation의 가용 replica, scaled-to-zero/가용 replica 부족, 이전 generation/누락 generation은 unknown. 가용성은 전달 증명이 아니다.
- 백업: 활성 Barman 설정 및 WAL 조건, 참조 serverName의 성공 시각만 선택, 오래됨·미래·잘못된 시각·권한 거부·ObjectStore 없음, 미지원 설정은 unknown.
- gate: 등록 환경의 factory만 사용. Plan의 kubeconfig/context 무시, DB 참조가 Plan target.cluster와 다르면 unknown, 다른 환경 설정을 상속하지 않음. 부분 pass와 end-to-end unknown을 감사 요약에 함께 보존.
- 3환경 템플릿: placeholder만 사용, 미등록 onprem 거부. 실제 연결 또는 운영 baseline이라고 주장하지 않음.

## 확인하지 않은 것

- 실제 EKS/GKE/k3s의 리소스 상태나 네트워크/플랫폼 자격 증명.
- Secret 값의 유효성·앱 사용·회전·암호화.
- k3s audit policy, 실제 감사 이벤트 전달·보존·앱 범위.
- 백업 객체/WAL 실재, 실제 restore 및 복구 데이터. 원본 dependency 자동 발견.
- PostgreSQL/MinIO/Helm 등의 opt-in 통합 테스트와 실제 마이그레이션 E2E는 별도 자원을 준비해 실행하지 않음. 로컬 전체 Go 검사 통과와 동등하지 않음.
- infra의 `PLATFORM_PARITY_FILE` 마운트/환경 변수는 변경하지 않음.
