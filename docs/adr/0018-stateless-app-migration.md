# 0018. 데이터베이스가 없는 앱은 게이트웨이 경로와 파일만 옮기고, 언제든 원본으로 되돌린다

- 상태: 채택
- 날짜: 2026-11-07
- 관련: [0002 고정 단계와 상태 파일](0002-fixed-steps-and-state-file.md), [0003 원복 경계](0003-rollback-boundary.md), [0005 게이트웨이 계약](0005-gateway-contract.md), [0012 cold 모드](0012-start-target-app-after-promotion.md), [0013 halt 재개](0013-shorter-write-pause-and-recoverable-halt.md), [0017 외부 원본](0017-external-source-switchover.md)

## 상황

팀은 10/31에 상태가 있는 앱과 없는 앱을 모두 옮길 수 있어야 한다고 정했다. docker-doom처럼 postgres 리소스가 없는 앱은 플랫폼이 EKS와 GKE에 모두 배포하지만, 엔진으로는 옮길 수 없다. Plan의 원본과 대상이 모두 CNPG Cluster(또는 0017의 외부 PostgreSQL)여야 하고, 모든 단계가 데이터베이스를 읽기 때문이다.

이런 앱에서 옮길 것은 게이트웨이 경로와, 있다면 오브젝트 스토리지 파일뿐이다. 양쪽 앱은 이미 떠 있고 서로 공유하는 상태가 없다.

## 결정

### Plan

```json
{
  "id": "doom-eks-to-gke",
  "source": {"kind": "none", "env": "eks"},
  "target": {"kind": "none", "env": "gke",
             "app": {"smokeUrl": "https://doom-gke.example", "smoke": [{"method": "GET", "path": "/"}]}},
  "gateway": {"mode": "http", "url": "${GATEWAY_URL}", "token": "${GATEWAY_TOKEN}"},
  "files": {"source": "src:doom", "target": "dst:doom"}
}
```

- `source.kind`와 `target.kind`가 둘 다 `none`이면 데이터베이스가 없는 plan이다. 한쪽만 `none`이면 거부한다. `external`과 섞을 수도 없다.
- 두 endpoint는 `env`만 쓴다. `namespace`, `cluster`, `dsn`, `kubeconfig`, `context`, `manifest`, `reindex`, `name`이 있으면 거부한다. 예외로 `target.app.mode`가 `cold`이면 대상 앱 workload를 찾기 위해 `target.namespace`(필수), `target.kubeconfig`, `target.context`를 받는다.
- `gateway.mode`는 `http`여야 한다. 경로를 옮기는 것이 곧 이전이기 때문이다.
- `verify.schemas`, `verify.tables`, `verify.writtenRecords`는 거부한다. `verify.fenceGate`는 생략하거나 `false`만 받고, `true`는 거부한다. 비교할 데이터베이스가 없는데 켜 두면 검증한 것처럼 보이기 때문이다. 생략하면 `false`로 채우고 `schemas` 기본값(`public`)은 채우지 않는다.
- `target.app`은 warm(기본)과 cold를 모두 받는다. warm에서도 `smokeUrl`과 `smoke`를 줄 수 있고, 주면 cold와 같은 규칙(GET 또는 HEAD, `/`로 시작하는 path)으로 검사한다.
- `files`는 지금과 같이 선택이다.
- `decommission`은 아직 거부한다. 지금의 decommission은 원본 Cluster와 DSN으로 옛 데이터베이스의 세션을 세고 막기 때문이다.
- `thresholds`는 `switchoverTimeout`(검증 smoke 반복과 원복 시간)과 `maxDowntime`(cold 기동 예산, cutover 경고)만 쓴다.

### 단계

단계 이름, 상태 파일, evidence 형식은 그대로 쓴다. 데이터베이스 필드(`fenceLsn`, `sourceDemoted`, `promoteRequested`)는 채우지 않는다.

| 단계 | 하는 일 |
| --- | --- |
| precheck | 게이트웨이 `/healthz`를 확인한다. files가 있으면 rclone이 있는지 보고 `rclone check`로 두 remote를 읽어 본다(차이는 실패가 아니다). cold면 대상 workload를 읽고, warm에 smoke가 있으면 대상 앱에 smoke를 한 번 보낸다. smoke가 없으면 대상 앱은 확인하지 않고 evidence에 남긴다. |
| prepare | cold면 대상 앱을 0으로 줄이고 pod가 없어질 때까지 기다린다. files가 있으면 첫 복사를 한다. |
| presync | 건너뛴다. |
| delta | files가 있으면 prepare 뒤 바뀐 파일을 복사한다. |
| fence | 게이트웨이에서 쓰기를 붙잡고, files가 있으면 마지막 복사를 한다. 쓰기 정지는 여기서 시작한다. |
| switchover | 건너뛴다. |
| start-target | cold면 0012와 같이 대상 앱을 띄우고 smoke가 통과할 때까지 기다린다. warm이면 건너뛴다. |
| cutover | 게이트웨이를 대상으로 돌리고 붙잡은 쓰기를 대상에서 푼다(공유 함수). |
| verify | files가 있으면 `rclone check --one-way`로 원본의 모든 파일이 대상에 있는지 본다. smoke가 있으면 대상 앱에 `switchoverTimeout` 동안 반복해 보낸다. |
| finalize | 쓰기 정지 시간과 대상 backend를 남긴다. |

DB 부하 예산(0016)은 적용하지 않는다.

### 원복

데이터베이스로 옮겨 간 데이터가 없으므로 원복은 게이트웨이를 원본으로 돌리고, 붙잡은 쓰기가 있으면 원본에서 푸는 것이다. 그래서 0003의 원복 경계가 없다.

- fence 전 실패는 run을 failed로 둔다. 바뀐 것이 없다.
- fence부터 cutover까지의 실패는 원본으로 경로를 돌리고 쓰기를 푼다.
- cutover로 쓰기를 푼 뒤의 실패(verify, finalize)도 원본으로 경로를 돌린다. cutover 뒤 대상의 files에 쓰인 파일은 원본으로 복사하지 않는다. rollback 이벤트에 그 사실을 남긴다.
- cold에서 start-target이 실패해도 대상 앱은 줄이지 않는다. 사용자는 원본만 보고, 다음 run의 prepare가 다시 줄인다.
- 원복의 게이트웨이 호출이 실패하면 halt한다. fence, start-target, cutover 중에 프로세스가 죽은 run도 지금처럼 halt한다(`checkRunnable`).
- halt한 run에 `-resume-halted`를 주면 원본으로 경로를 돌리고 쓰기를 풀어 원복을 마친다. 붙잡기나 경로 변경이 저장되지 않았을 수 있으므로 중단된 단계가 있으면 쓰기를 푸는 호출까지 한다. 두 호출은 반복해도 안전하다. 결과는 `ErrResumeRolledBackStateless`이고 run은 rolled_back으로 끝난다.

성공한 run을 나중에 되돌리는 명령은 만들지 않았다. 같은 plan의 원본과 대상을 바꾼 plan을 새로 돌리면 된다.

## 검토한 대안

- 원본과 대상 kind를 나누지 않고 Plan 최상위에 `stateless: true`를 둔다. 원본과 대상이 각자 무엇인지 말하는 0017의 `kind`와 어긋나고, 한쪽에만 데이터베이스가 있는 경우를 막는 규칙을 따로 써야 해서 쓰지 않았다.
- switchover 단계에서 경로를 옮긴다. 경로 변경과 쓰기 해제는 이미 cutover가 하고, cold 앱 기동이 그 사이에 와야 하므로 cutover를 그대로 썼다.
- decommission을 지원한다. 원본 앱 정지와 smoke는 쓸 수 있지만 지금 구현은 옛 데이터베이스 세션 관찰과 차단에 묶여 있어 따로 다룬다.

## 결과

- 플랫폼은 postgres 리소스가 없는 앱에 위 Plan을 만들어 같은 `run`으로 옮길 수 있다.
- 쓰기 정지 시간은 마지막 파일 복사와 게이트웨이 호출, cold면 앱 기동 시간이다.
- CNPG 경로와 외부 원본 경로의 동작은 바뀌지 않는다. 테스트는 가짜 포트로 성공, cutover 전 실패의 원복, cutover 뒤 원복, halt 재개를 확인한다.
