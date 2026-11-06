# 2026-11-06: 파일 본문 기반 레지던시 검사 — 구현·검증 기록

## 대상과 환경

- 대상: `platform/internal/fileguard`, `internal/preflight`, `internal/runs`, `internal/plans`, `internal/api`, `cmd/api` 및 정책 예제/ADR.
- 초기 검증의 platform base commit: `b7e907ac1d1bd4ea81a977946ad2cb162142d3b8` (`main`). 초기 검증은 미커밋 변경으로 실행했으며, 이후 사용자의 PR 요청에 따라 최신 main 위 feature commit `7b6886045f7383e595bbe4ec980e0e711e1b20cb`를 만들고 재검증했다(아래 PR 준비 기록).
- 기존 migration-engine base: `a1dc35c`; 기존 test-records 변경과 parity 기록/로그는 보존하고 이번 기록만 추가했다.
- Windows 11, PowerShell 5.1 및 Git Bash. 실제 Go는 **go1.26.7 windows/amd64**, CGO_ENABLED=1, CC=gcc. [도구 확인](2026-11-06-file-content-inspection-toolchain.log), [race 도구 확인](2026-11-06-file-content-inspection-race-toolchain.log).
- Go/gofmt가 PowerShell PATH에 없었으나 `C:\Program Files\Go\bin`에 설치되어 있었다. 명령별 PATH만 추가했다. [초기 PATH 오류 기록](2026-11-06-file-content-inspection-path-discovery.log).
- rclone은 기존 PATH에 없었다. 공식 `https://downloads.rclone.org/v1.70.3/`의 Windows amd64 portable archive를 세션 scratchpad의 새 격리 디렉터리에 내려받아 같은 공식 HTTPS `SHA256SUMS`와 대조했다. SHA-256: `1c75923b4d3f0b3d1faf16447a442b5aaed0cfc32997b3381eb96fd087603a30`. 시스템 설치/전역 PATH 변경은 하지 않았다. [다운로드·검증 로그](2026-11-06-file-content-inspection-rclone-download.log).
- 실제 로컬 통합에 사용한 바이너리는 **rclone v1.70.3**(바이너리 자체는 Go 1.24.4 빌드)다. 플랫폼 테스트의 Go 1.26.7과 구분한다.
- 운영 클러스터/실제 클라우드 버킷 접속과 배포는 수행하지 않았다. 초기 검증 중에는 commit/push도 하지 않았고, 이후 사용자가 PR 작성을 요청해 feature branch와 커밋을 준비했다. 실제 rclone 통합의 입력/출력은 Go 테스트가 만든 로컬 임시 디렉터리다.

## 구현한 경로

- 원본 전체 객체 열거 → private staging 다운로드 → 본문 검사/SHA-256 manifest → 기존 Rego 분류에 추가 제약 → 검사한 staging만 전송.
- UTF-8 원문/제한된 ZIP32, 중첩/팽창/경로/CRC/미지원 형식 제한 및 정확한 SHA-256 기반 운영자 검토 분류.
- 최초 시작 검사와 정상 run의 매 Sync/Check, 재개 시 어댑터 주입. 기존 역방향 롤백 복구 면제 유지.
- KR 내 검사 저장소를 선언하는 `inspectionEnvironment`가 없으면 본문 다운로드 전에 거부.
- 안전한 요약 증거만 감사 로그/엔진 evidence에 기록. 원문·키·일치 문자열·원시 프로세스 오류는 제외.
- 설계/한계: `platform/docs/adr/0031-file-content-residency.md`.

## 실제 실행 결과

아래 명령은 platform 모듈을 대상으로 실행했다. `go -C platform` 또는 해당 검증 복사본의 `go -C <path>`를 사용했다. 기본 빌드 태그의 전체 suite 통과는 선택적 외부 통합/태그 테스트의 실행을 뜻하지 않는다.

| 확인 | 결과 | 원시 기록 |
|---|---|---|
| 변경 파일 `gofmt -w` | 통과, 실제 포맷 적용 | [format retry](2026-11-06-file-content-inspection-format-retry.log) |
| 변경/신규 Go 19개 `gofmt -l`, 두 repo `git diff --check` | 통과. Git의 LF→CRLF 경고는 있으나 포맷 차이/공백 오류 없음 | [format check](2026-11-06-file-content-inspection-format-check-01.log) |
| `go build ./...` | **통과**, exit 0 | [build](2026-11-06-file-content-inspection-build-01.log) |
| `go vet ./...` | **통과**, exit 0 | [vet](2026-11-06-file-content-inspection-vet-01.log) |
| targeted `go test -count=1` 1회차 | **실패**, exit 1. 새 fileguard/preflight/runs/plans 및 cmd/api는 통과; 기존 API 배포 순서 테스트 실패 | [targeted 01](2026-11-06-file-content-inspection-targeted-01.log) |
| 작업 디렉터리 `go test -count=1 ./...` | **실패**, exit 1. API 포함 나머지는 통과, `tools/apidoc.TestRenderRepoContract`의 CRLF 입력 문제 | [full 01](2026-11-06-file-content-inspection-full-01.log) |
| pristine base `TestListAndGetDeployments -count=20` | 통과. 간헐 실패라 이 실행에서는 재현되지 않음 | [baseline 01](2026-11-06-file-content-inspection-baseline-api.log) |
| pristine base 같은 테스트 `-count=200` | **동일 실패 재현**, exit 1. 변경 전 코드에서도 생성 시각 동률 시 기대 순서 실패 | [baseline 02](2026-11-06-file-content-inspection-baseline-api-02.log) |
| 실제 `rclone version` + `TestLocalRcloneIntegration -v -count=1` | **통과, skip 아님**. 실제 로컬 복사/비교/PII 거부 후 이전 대상 바이트 유지 | [local rclone](2026-11-06-file-content-inspection-local-rclone-01.log) |
| `go test -count=1 -cover -v ./internal/fileguard` (rclone 사용 가능) | **통과**, coverage **81.3%**. 로컬 통합도 PASS | [fileguard verbose](2026-11-06-file-content-inspection-fileguard-verbose.log) |
| LF 복사본 `go test -count=1 ./...` (rclone 사용 가능) | **전체 통과**, exit 0, apidoc 포함 | [LF full](2026-11-06-file-content-inspection-lf-full-01.log) |
| LF 복사본 관련 패키지 `go test -race -count=1` | **통과**, exit 0. race detector 보고 없음 | [race](2026-11-06-file-content-inspection-race-01.log) |
| 작업 디렉터리 targeted `go test -count=1` 2회차 (rclone 사용 가능) | **전체 통과**, exit 0. 첫 실패를 지우지 않고 재확인 결과 보존 | [targeted 02](2026-11-06-file-content-inspection-targeted-02.log) |
| `go test -tags minio -run '^$' ./internal/preflight/gate ./internal/preflight/bucketprobe` | **컴파일만 통과**, `[no tests to run]`. 실제 MinIO 실행/접속 아님 | [MinIO compile](2026-11-06-file-content-inspection-minio-compile.log) |
| 문서 갱신 후 최종 소스/포맷/상태 점검 | **통과**, exit 0. Go/Rego/module 285개가 테스트한 LF 복사본과 동일, 변경 Go 19개 포맷 정상, 두 repo diff --check 정상, 기존 parity 기록 보존 | [final check](2026-11-06-file-content-inspection-final-check.log) |

Targeted와 race의 대상은 동일하게 다음 패키지를 포함했다.

```sh
go test -count=1 ./internal/fileguard ./internal/preflight/... ./internal/runs ./internal/plans ./internal/api ./cmd/api
go test -race -count=1 ./internal/fileguard ./internal/preflight/... ./internal/runs ./internal/plans ./internal/api ./cmd/api
go build ./...
go vet ./...
go test -count=1 ./...
go test -count=1 -cover -v ./internal/fileguard
go test -count=1 -v -run '^TestLocalRcloneIntegration$' ./internal/fileguard
go test -tags minio -run '^$' ./internal/preflight/gate ./internal/preflight/bucketprobe
```

## 실패 원인 분리와 LF 검증 방법

1. 첫 targeted 실패는 `internal/api/deploy_test.go:182`, `TestListAndGetDeployments`였다. 로그에서 두 deployment의 `createdAt`이 같았고, register/manual 순서가 테스트 기대와 달랐다. 이번 변경이 없는 `git archive HEAD`의 pristine base에서도 200회 반복으로 동일 실패를 재현했다(앞선 20회에서는 미재현). 이 테스트와 배포 정렬 로직은 이번 작업에서 수정하지 않았다. 현재도 간헐 실패 가능성이 남는다.
2. 작업 디렉터리의 전체 실행은 `tools/apidoc/main_test.go:53`, `chunk 1 is 153518 bytes, over 20000`로 실패했다. 기존 CRLF 파일을 바꾸거나 테스트를 무시하지 않고, 같은 현재 tracked+untracked(non-ignored) 파일을 scratchpad의 새 복사본으로 옮겼다. UTF-8 텍스트의 CRLF만 LF로 정규화하고 바이너리는 그대로 보존했다. 이 복사본에서 apidoc 포함 전체 suite가 통과했다.
3. [snapshot 로그](2026-11-06-file-content-inspection-snapshot.log)에 base hash, 복사본 경로, 정규화 목록, 414개 파일 및 정규화 후 파일 집합 manifest SHA-256 `f8e72e1c0dae8b385e7c7ed714e40002bbd4e0935d0c5a70b1cbb1818da593dd`를 남겼다. 이는 검증 복사본의 provenance이지 런타임 fileguard manifest가 아니다. 검증 후에는 문서/기록만 갱신했고 Go/Rego 구현은 변경하지 않았다.
4. LF 전체/race도 **Windows Go**로 실행했다. Linux/container/CI 검증이라고 부르지 않는다. 실제 working tree의 CRLF apidoc 실패가 해결되었다고 주장하지 않는다.

## 검사별 검증 범위

- `internal/fileguard/scan_test.go`: 실제 텍스트/ZIP/중첩 검사, 탐지·증거 비식별화, exact hash/stale review, 미지원 형식, 경로/CRC/extra/trailer/깊이/객체 크기, 키/바이트에 묶인 manifest.
- `internal/fileguard/transfer_test.go`: synthetic transport + 실제 staging/scanner로 검사 후 원격 동일 크기 변경, staging 변조, 다음 패스의 새 PII, 거부 후 업로드 없음, 경로/수/크기/전송 오류 redaction, rclone 전역 환경 옵션 제거.
- `TestLocalRcloneIntegration`: 실제 rclone + 로컬 디렉터리 복사/검증/거부를 실행해 통과. 위 synthetic transport 테스트와 구분한다.
- `internal/preflight/residency/content_test.go`: 실제 OPA에서 내용 분류가 기존 제약을 완화하지 않는지, onward replica 제한, label 검증, reviewedFiles 정규화/digest/설정 검증.
- `internal/preflight/gate/content_test.go`: synthetic inspector로 시작 경로의 추가 분류/검사 실패/검사 위치/옵션 거부, 운영자 review 복사 확인.
- `internal/runs/content_test.go`: start/resume/halted-resume 어댑터 연결, rollback 면제, nil 어댑터의 raw rclone fallback 차단. Factory에서 의도적으로 중단하므로 실제 엔진 이전 증거가 아님.
- `internal/api/residency_content_test.go`: 허용/거부 감사 로그의 content evidence 배선. synthetic verdict이며 실제 scanner/운영 이전 증거가 아님.
- 기존 gate metadata unit tests는 test inspector로 격리했다. `minio` build-tag 테스트는 실제 rclone 본문 검사를 사용하도록 조정하고 컴파일만 확인했다.

## 최초 도구 차단 기록 (보존)

- [최초 test 호출](2026-11-06-file-content-inspection-test.log), [최초 gofmt 호출](2026-11-06-file-content-inspection-format.log)은 자동 안전 검사 서비스 timeout으로 **프로세스 시작 전에 차단된 시도**다.
- 당시 오류: `claude-sonnet-5 is temporarily unavailable (timed out), so auto mode cannot determine the safety of Bash right now.` PowerShell/설정 도구도 같은 종류의 오류였다.
- 이 두 로그는 subprocess stdout이 아니라 호출/도구 오류를 기록한 것이다. 해당 시도에 종료 코드나 pass/fail/skip 수를 붙이지 않는다.
- 사용자가 manual permission mode로 전환한 이후 셸 실행이 가능해졌다. 별도 permission 설정 파일 생성이나 보호 장치 우회는 하지 않았다. 이후 실제 실행 결과는 위 별도 로그들이다.

## PR 준비: 최신 main 재배치 및 재검증

- 사용자의 PR 요청 후 `platform/origin/main`에 새 커밋 3개가 있어 feature branch를 최신 base `eba119b3f6c3230aac3cd2ab0cf27c2c67e6fddd` 위로 rebase했다. 최종 구현 커밋은 `7b6886045f7383e595bbe4ec980e0e711e1b20cb`다. 충돌 없이 재배치되었다.
- 최신 main이 ADR 0030을 사용하므로 이번 ADR과 링크는 **0031-file-content-residency.md**로 조정했다. 과거 로그의 0030 경로는 당시 출력이므로 수정하지 않았다.
- 최신 main의 migration-engine pin은 `v0.0.0-20261106053317-a1dc35c2261c`다. 이번 feature가 임의로 엔진 버전을 변경한 것은 아니다.
- 정확한 PR 커밋을 `git -c core.autocrlf=false archive`로 새 scratchpad에 내보내 변경 Go 19개의 gofmt를 확인했다. 첫 archive 시도는 core.autocrlf=true의 출력 변환 때문에 포맷 검사에서 실패했고, 변환을 끈 재시도에서 통과했다. 전역/저장소 config를 바꾸지 않았다. [첫 시도](2026-11-06-file-content-inspection-pr-snapshot.log), [성공 snapshot/commit/format](2026-11-06-file-content-inspection-pr-snapshot-02.log).
- PR 커밋의 `go build ./...`와 `go vet ./...`는 통과했다. 이어진 전체 `go test -count=1 ./...`의 첫 실행은 이미 원본에서도 재현한 `TestListAndGetDeployments` 생성 시각 동률로 실패했다. apidoc와 새 검사 관련 패키지는 통과했다. [첫 build/vet/full](2026-11-06-file-content-inspection-pr-full.log).
- 같은 PR 커밋에서 전체 `go test -count=1 ./...`를 재실행해 **전체 통과**, exit 0. 실패 로그를 지우지 않고 보존한다. [전체 재확인](2026-11-06-file-content-inspection-pr-full-02.log).
- 같은 PR 커밋에서 관련 패키지 `go test -race -count=1 ./internal/fileguard ./internal/preflight/... ./internal/runs ./internal/plans ./internal/api ./cmd/api`도 **통과**, exit 0. [PR race](2026-11-06-file-content-inspection-pr-race.log).
- 위 PR 재검증도 Windows Go 1.26.7 + LF 파일 + 실제 portable rclone PATH를 사용했다. Linux/원격 CI 결과가 아니다.
- 첫 PR snapshot 실패의 원시 로그는 stderr 마지막 빈 줄까지 보존한다. 새 파일을 stage한 뒤 `git diff --cached --check`는 해당 `.log`의 EOF 빈 줄만 경고했다. 원시 출력을 고치지 않고 두 Markdown 문서의 공백 검사를 별도로 통과시켰다.
- 기록 PR에는 이번 `2026-11-06-file-content-inspection*` 파일과 README의 해당 행만 포함한다. 작업 전부터 남아 있던 parity README 행과 16개 parity 파일은 stage/commit하지 않고 보존한다.

## 남은 미검증 및 운영 경계

- 실제 MinIO/S3/GCS body read/transfer, 운영 API → 승인 → run 전체 연결과 네트워크 실패, 운영 delta/fence 도중 새 개인정보가 들어오는 시나리오. 로컬/synthetic 테스트가 이 검증을 대신하지 않는다.
- KR staging 실제 배치/Windows 서비스 계정 TEMP ACL/복제/백업/swap/core dump/프로세스 강제 종료 후 잔여물 통제. `inspectionEnvironment`는 운영자 선언이다.
- 전체 스캔/재전송이 fence 시간·이전 비용에 주는 영향. 무중단 성능/대규모 부하를 검증하지 않았다.
- 메타데이터·Content-Type 의미 보존이 필요한 앱은 초기 경로의 운영 적용 대상이 아니다. ZIP/OCR/DLP 한계와 rollback/CLI/policy-disabled 면제도 남는다.
- 최종 fence의 전체 객체 writer 정지, 목록/다운로드의 원자적 스냅샷, 버킷 DNS/복제 메타데이터 TOCTOU는 증명하지 않는다.
- 기본 suite에서 선택적 외부 통합의 개별 skip 수를 집계하지 않았다. MinIO 태그는 컴파일 확인만 했고 Linux 실행/원격 CI는 하지 않았다.

**현재 상태: gofmt·컴파일·로컬 targeted/전체(LF)·관련 race·vet·실제 로컬 rclone 검증 완료. 운영 이전 가능성이 입증되었거나 운영 반영 승인을 받은 상태는 아니다.**
