# 0007. 엔진은 포트 인터페이스에 의존하고, 가짜 topology로 테스트한다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

첫 버전의 엔진은 CNPG, PostgreSQL, rclone 어댑터를 직접 만들어 썼다. 그래서 원복 경계 같은 핵심 동작을 테스트하려면 클러스터 두 개와 DB가 있어야 했고, 엔진 테스트 커버리지는 1.4%였다. 원복 경계가 틀리면 split-brain이 나므로 이 부분은 반드시 테스트로 지켜야 한다.

## 결정

- 엔진은 작은 인터페이스에만 의존한다(`pkg/engine/ports.go`).
  - `Cluster`: CNPG Cluster 조회, apply, primary 변경, 대기
  - `Database`: recovery 여부, WAL 위치, 지연
  - `Connector`: DSN으로 `Database` 열기
  - `FileSyncer`: rclone sync, check
  - `Verifier`: 테이블 비교, 누락 행, k6 기록 대조
- `Options`로 각 포트와 폴링 간격을 바꿀 수 있다. 비워 두면 실제 어댑터를 쓴다. CLI 동작은 그대로다.
- 어댑터는 상태 파일을 쓰기 전에 만든다. kubeconfig가 틀려도 빈 run이 남지 않는다.
- 테스트는 demote와 promote에 반응하는 가짜 topology("world")로 한다. 정상 경로의 호출 순서, 실패 지점 아홉 곳의 결과(`failed`, `rolled_back`, `halted`), 원복 실패, 다운타임 예산, 재개를 확인한다.
- 어댑터는 따로 테스트한다. 게이트웨이는 `httptest`로, CNPG는 fake dynamic client로 status 해석과 patch 내용을 확인한다.

## 검토한 대안

- 실제 클러스터로만 테스트: 가장 정확하지만 느리고, 실패 지점을 마음대로 만들기 어렵다.
- 컨테이너 PostgreSQL만 붙이기: DB 동작은 확인하지만 CNPG의 demote/promote는 흉내 낼 수 없다.

## 결과

- 커버리지는 engine 74.6%, cnpg 93.4%, gateway 93.3%다.
- 원복 경계를 일부러 틀리게 바꾼 버그 네 가지를 테스트가 모두 잡는 것을 확인했다.
- 가짜 topology 통과는 실제 이전이 된다는 증거가 아니다. `pg`와 `verify`는 Docker로 띄운 primary와 스트리밍 standby에서 따로 테스트한다(`test/integration/run.sh`, 빌드 태그 `integration`). 실제 CNPG 리허설(0001)은 그것과도 별개다.
- `-race`는 cgo가 없는 환경에서 돌리지 못했다.
