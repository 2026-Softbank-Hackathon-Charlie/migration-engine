# 0020. 데이터베이스가 없는 앱은 게이트웨이 경로와 파일만 옮기고, 언제든 원본으로 되돌린다

- 상태: 채택
- 날짜: 2026-11-07
- 관련: [0002 고정 단계와 상태 파일](0002-fixed-steps-and-state-file.md), [0003 원복 경계](0003-rollback-boundary.md), [0005 게이트웨이 계약](0005-gateway-contract.md), [0012 cold 모드](0012-start-target-app-after-promotion.md), [0013 halt 재개](0013-shorter-write-pause-and-recoverable-halt.md), [0019 외부 원본](0019-external-source-switchover.md)

## 상황

팀은 10/31에 상태가 있는 앱과 없는 앱을 모두 옮길 수 있어야 한다고 정했다. docker-doom처럼 postgres 리소스가 없는 앱은 플랫폼이 EKS와 GKE에 모두 배포하지만, 엔진으로는 옮길 수 없다. Plan의 원본과 대상이 모두 CNPG Cluster(또는 0019의 외부 PostgreSQL)여야 하고, 모든 단계가 데이터베이스를 읽기 때문이다.

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
- `gateway.mode`는 `http`여야 한다. 경로를 옮기는 것이 곧 이전이기 때문이다. `gateway.sourceBackend`와 `targetBackend`가 같으면 경로가 움직이지 않으므로 거부한다.
- `verify.schemas`, `verify.tables`, `verify.writtenRecords`는 거부한다. `verify.fenceGate`는 생략하거나 `false`만 받고, `true`는 거부한다. 비교할 데이터베이스가 없는데 켜 두면 검증한 것처럼 보이기 때문이다. 생략하면 `false`로 채우고 `schemas` 기본값(`public`)은 채우지 않는다.
- `target.app`은 warm(기본)과 cold를 모두 받는다. warm에서도 `smokeUrl`과 `smoke`를 줄 수 있고, 주면 cold와 같은 규칙(GET 또는 HEAD, `/`로 시작하는 path)으로 검사한다. warm에 `kind`, `name`, `replicas`가 있으면 거부한다. warm 앱은 엔진이 늘리거나 줄이지 않는다.
- `files`는 지금과 같이 선택이다.
- `decommission`은 아직 거부한다. 지금의 decommission은 원본 Cluster와 DSN으로 옛 데이터베이스의 세션을 세고 막기 때문이다.
- `thresholds`는 `switchoverTimeout`(검증 smoke 반복과 원복 시간)과 `maxDowntime`(fence의 마지막 파일 복사 제한, cold 기동 예산, cutover 경고)만 쓴다.

### 단계

단계 이름, 상태 파일, evidence 형식은 그대로 쓴다. 데이터베이스 필드(`fenceLsn`, `sourceDemoted`, `promoteRequested`)는 채우지 않는다.

| 단계 | 하는 일 |
| --- | --- |
| precheck | 게이트웨이 `/healthz`를 확인한다. files가 있으면 rclone이 있는지 보고 원본만 `rclone lsf --max-depth 1`로 읽어 본다. 대상 경로는 첫 이전 전에 없을 수 있고 첫 복사가 만든다. cold면 대상 workload를 읽고, warm에 smoke가 있으면 대상 앱에 smoke를 한 번 보낸다. smoke가 없으면 대상 앱은 확인하지 않고 evidence에 남긴다. |
| prepare | cold면 대상 앱을 0으로 줄이고 pod가 없어질 때까지 기다린다. files가 있으면 `rclone copy`로 첫 복사를 한다. |
| presync | 건너뛴다. |
| delta | files가 있으면 prepare 뒤 바뀐 파일을 복사한다. |
| fence | 게이트웨이에서 쓰기를 붙잡고, files가 있으면 마지막 복사를 한다. 이 복사는 `maxDowntime` 안에 끝나야 하고, 넘으면 실패해 원복한다. 쓰기 정지는 여기서 시작한다. |
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
- cutover로 쓰기를 푼 뒤의 실패(verify, finalize)는 files가 없는 plan이면 원본으로 경로를 돌린다. 사용자가 보기에는 경로가 두 번 바뀌므로, rollback 이벤트에 "second traffic flip"이라고 남긴다.
- files가 있는 plan은 cutover 뒤 실패에서 자동으로 되돌리지 않고, 경로를 대상에 둔 채(쓰기는 풀린 상태) halt한다. 그 사이 사용자가 대상에 올린 파일이 원본에는 없기 때문이다. 운영자가 `-resume-halted`를 주면 원본으로 경로를 돌린다. 대상에만 있는 파일이 필요하면 그 전에 `rclone copy <target> <source>`로 옮겨 둔다. 엔진은 이 역방향 복사를 하지 않는다.
- 호출한 쪽의 context가 cutover 뒤(verify, finalize)에 취소되면(Ctrl-C) 원복하지 않고 halt한다. 이전은 이미 끝났기 때문이다. 이 halt에 `-resume-halted`를 주면 남은 단계를 다시 돌린다.
- cold에서 start-target이 실패해 원복해도 대상 앱은 줄이지 않고 떠 있는 채로 둔다. 사용자는 원본만 보고, 다음 run의 prepare가 다시 줄인다.
- 원복의 게이트웨이 호출이 실패하면 halt한다. fence, start-target, cutover 중에 프로세스가 죽은 run도 지금처럼 halt한다(`checkRunnable`).
- halt한 run에 `-resume-halted`를 주면 원본으로 경로를 돌리고 쓰기를 풀어 원복을 마친다. 붙잡기나 경로 변경이 저장되지 않았을 수 있으므로 중단된 단계가 있으면 쓰기를 푸는 호출까지 한다. 두 호출은 반복해도 안전하다. 결과는 `ErrResumeRolledBackStateless`이고 `errors.Is`로 `ErrResumeRolledBack`과도 맞는다. run은 rolled_back으로 끝난다. cutover 뒤 취소로 멈춘 run만 예외로 남은 단계를 다시 돌린다.

### 파일 복사

stateless plan의 파일 복사(prepare, delta, fence)는 `rclone sync`가 아니라 `rclone copy`를 쓴다. sync는 원본에 없는 대상 객체를 지우므로, 앞선 이전 뒤 대상에 올라온 파일이나 cutover 뒤 원복하고 다시 돌린 run에서 사용자가 올린 파일을 지운다. copy는 새로 생기거나 바뀐 객체만 옮기고 대상에서 지우지 않는다. 그래서 원본에서 지운 파일은 대상에 남는다. verify의 `rclone check --one-way`는 대상에만 있는 객체를 세지 않으므로 이것으로 실패하지 않는다. CNPG와 외부 원본 경로는 지금처럼 sync를 쓴다.

### 게이트웨이 쓰기 정지의 범위

게이트웨이의 쓰기 정지는 쓰기 요청(GET과 HEAD가 아닌 요청)을 붙잡는다. WebSocket은 GET으로 업그레이드하므로 붙잡히지 않는다. docker-doom의 VNC처럼 이미 열린 WebSocket 세션은 경로가 바뀔 때 게이트웨이 구현에 따라 끊기거나 원본에 붙은 채 남는다. 남은 세션이 원본에 쓰는 파일은 fence 뒤라 대상에 복사되지 않는다. 세션 안의 상태(게임 진행 등)는 옮기지 않으며, 사용자는 다시 접속해 대상에서 새로 시작한다.

성공한 run을 나중에 되돌리는 명령은 만들지 않았다. 같은 plan의 원본과 대상을 바꾼 plan을 새로 돌리면 된다.

## 검토한 대안

- 원본과 대상 kind를 나누지 않고 Plan 최상위에 `stateless: true`를 둔다. 원본과 대상이 각자 무엇인지 말하는 0019의 `kind`와 어긋나고, 한쪽에만 데이터베이스가 있는 경우를 막는 규칙을 따로 써야 해서 쓰지 않았다.
- switchover 단계에서 경로를 옮긴다. 경로 변경과 쓰기 해제는 이미 cutover가 하고, cold 앱 기동이 그 사이에 와야 하므로 cutover를 그대로 썼다.
- decommission을 지원한다. 원본 앱 정지와 smoke는 쓸 수 있지만 지금 구현은 옛 데이터베이스 세션 관찰과 차단에 묶여 있어 따로 다룬다.

## 결과

- 플랫폼은 postgres 리소스가 없는 앱에 위 Plan을 만들어 같은 `run`으로 옮길 수 있다.
- 쓰기 정지 시간은 마지막 파일 복사와 게이트웨이 호출, cold면 앱 기동 시간이다.
- CNPG 경로와 외부 원본 경로의 동작은 바뀌지 않는다. 테스트는 가짜 포트로 성공, cutover 전 실패의 원복, cutover 뒤 원복, halt 재개를 확인한다.
