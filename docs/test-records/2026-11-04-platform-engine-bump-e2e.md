# 2026-11-04 platform의 엔진 의존성 올림 후 실패 시나리오 e2e

- 대상: platform `51e02f9`(main) + 브랜치 `chore/bump-migration-engine`: migration-engine `f3b9fd8` → `c336658`(엔진 PR #5~#8 반영)
- 환경: platform `test/e2e/run.sh`, Windows 11, Docker Desktop, kind `hop-e2e`(새로 생성), CNPG 1.25.1, 쓰기 4·읽기 4 사용자 부하, 카나리 단계마다 5초
- 원시 로그(비밀번호·토큰 가림): [2026-11-04-platform-engine-bump-e2e.log](2026-11-04-platform-engine-bump-e2e.log)
- 전체 336초, 종료 코드 0

| 시나리오 | 결과 |
|---|---|
| canary-fault | 통과. 1% inconclusive(요청 4) → 10% failed(오류율 97.83%) → rolled_back. 게이트웨이 쓰기·읽기 src, 쓰기 5,280건 실패 0, 원본에 전부 있음 |
| observe-rollback | 통과. observe violated(p95 1,464ms) → `slo-guard` 롤백 run succeeded(다운타임 14.3초). 쓰기 1,314건 실패 0, 원본에 전부 있음 |
| migrate | 통과. 1% inconclusive(요청 3) → 10~100% ok, 다운타임 14.8초, 게이트웨이 실패 0, missingWrites 0, 기록한 쓰기 3,102건 전부 새 primary에 있음 |

- 단위: `go build`, `go vet ./...` 통과. `go test ./...`는 `tools/apidoc`만 실패(로컬 전용 기존 실패). `-race` internal/runs·api 통과
- 엔진 올림 전(같은 날 오후) 결과와 같은 판정. 엔진 #7(원복 시 재시작 대기)·#8(적용 안 된 강등 요청 원복)이 직접 걸리는 장애는 이 e2e에 없음(엔진 단위 테스트와 엔진 게이트웨이 리허설이 다룸)
- 검증하지 않은 것: 실제 EKS/GKE, 각 시나리오 1회
