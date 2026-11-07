# 0023. 기존 Go와 실제 CNPG CI를 관리자에게도 main 머지 필수 조건으로 강제한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

`.github/workflows/ci.yml`에는 이미 `pull_request`와 `push` main에서 실행되는 `go`, `cnpg-write-fence` job이 있었다. PR #30에서도 전체 race/vet/build·PostgreSQL primary/standby와 실제 kind CNPG fence 검사가 통과했다. CI의 부재가 아니라 GitHub main 보호 설정의 부재가 문제였다.

2026-11-08 확인 시 저장소 `2026-Softbank-Hackathon-Charlie/migration-engine`(REST ID `1441986954`, public)의 main protection은 `404 Branch not protected`, rulesets와 main에 적용되는 rules는 모두 빈 배열이었다. 체크가 실패하거나 실행 중이어도 GitHub가 머지를 차단하지 않았다.

## 결정

사용자의 CI 머지 차단 요청에 따라 2026-11-08 **02:59:16 KST**에 main 한 곳에 `.github/branch-protection/main.json`의 설정을 적용하고 GET으로 실효 값을 확인했다.

- `go`, `cnpg-write-fence` 두 체크가 성공해야 한다. 실제 체크를 발행한 GitHub Actions App ID `15368`로 제공자를 고정한다.
- `strict=true`로 최신 main과 동기화한 결과를 요구한다.
- PR 경유는 필수로 하되 별도의 승인 리뷰어 수는 0명으로 둔다. 이번 요청과 무관한 CODEOWNERS·리뷰 승인 정책은 추가하지 않는다.
- 관리자도 같은 체크를 통과해야 하며 PR bypass 명단을 추가하지 않는다. main force push와 삭제를 금지한다.
- 기존 workflow를 중복 생성하지 않는다. merge queue는 현재 구성하지 않았으므로 `merge_group` trigger도 추가하지 않는다. 나중에 queue를 활성화하면 해당 trigger를 함께 검토한다.

## 검토한 대안

- workflow만 추가하거나 중복 CI 생성: 기존의 필수 체크 부재를 해결하지 못한다.
- 관리자 우회 또는 모든 App이 발행한 같은 이름의 체크 허용: CI 차단을 쉽게 건너뛸 수 있어 제외했다.
- 새 리뷰어·CODEOWNERS 정책까지 강제: 별도의 팀 운영 변경이므로 이번 범위에서 제외했다.

## 결과

설정은 코드 파일만으로 활성화되지 않는다. GitHub 저장소 설정으로 적용했으며 JSON 파일은 검토 가능한 정확한 요청의 기록이다. 재적용은 자동으로 수행하지 않는다. 현재 보호나 organization rule이 생겼으면 먼저 비교하고, 이 파일로 덮어쓰지 않는다.

다음 명령으로 서버의 실효 설정과 실제 workflow를 확인할 수 있다. 출력에는 API token을 포함하지 않는다.

```sh
gh api repos/2026-Softbank-Hackathon-Charlie/migration-engine/branches/main/protection
gh api repos/2026-Softbank-Hackathon-Charlie/migration-engine/rules/branches/main
gh api repos/2026-Softbank-Hackathon-Charlie/migration-engine/actions/workflows
gh pr checks <PR번호> -R 2026-Softbank-Hackathon-Charlie/migration-engine
```

`go`나 `cnpg-write-fence` job 이름을 바꿀 때는 요구 체크도 함께 바꿔야 한다. 이 보호는 CI 성공을 강제하며 코드의 품질 자체나 관리자에 의한 저장소 설정 변경까지 불가능하게 만드는 통제는 아니다. 적용 전 보호 설정이 없음을 재확인했고 기존 rule·권한·리소스는 삭제하지 않았다.

공식 API 계약: [GitHub Update branch protection](https://docs.github.com/en/rest/branches/branch-protection#update-branch-protection).
