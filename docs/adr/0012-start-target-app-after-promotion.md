# 0012. 기동할 때 스키마를 바꾸는 앱은 대상 승격 뒤에 띄우고, 그 실패는 원복한다

- 상태: 채택 (엔진 구현 2026-11-05, platform·infra 후속 남음)
- 날짜: 2026-11-04

## 상황

지금 이전은 대상 앱이 전환 전부터 대상 DB에 붙어 떠 있다고 가정한다. 카나리(platform ADR 0005)가 읽기를 대상 앱으로 보내 보려면 대상 앱이 먼저 떠 있어야 하기 때문이다. 이때 대상 DB는 원본을 따라가는 읽기 전용 replica다(0001).

많은 앱은 시작할 때마다 스키마 마이그레이션 도구(Prisma, Flyway, golang-migrate 등)를 돌린다. 할 일이 없어도 기록 테이블에 잠금을 걸거나 한 줄을 쓰므로 replica에서는 `cannot execute ... in a read-only transaction` 같은 오류로 기동에 실패한다. 우리 샘플 앱 moimroom도 같은 문제가 있어서 앱 코드를 고쳤다(platform `test/e2e/moimroom/README.md`).

우리가 만든 앱은 고치면 되지만, 남이 만든 앱(오픈소스 등)에 "replica 위에서 뜰 수 있게 고쳐라"를 요구하면 플랫폼 주제("Frictionless")와 맞지 않는다. 기동할 때 마이그레이션을 끄는 옵션이 없는 앱도 앱 수정 없이 옮길 수 있어야 한다.

확인한 사실:

- 엔진의 원복 경계는 `promoteRequested`다(0003). 승격 요청 뒤의 실패는 `halted`로 멈춘다. 대상이 승격해 쓰기를 받았을 수 있어서다.
- 게이트웨이는 fence부터 cutover까지 사용자 쓰기를 붙잡는다(0005). 붙잡힌 쓰기는 원본에도 대상에도 가지 않는다.
- 승격 뒤 CNPG가 새 primary를 제자리 재시작하므로 엔진은 settle을 기다린다(0009). 2026-11-04 moimroom e2e에서 쓰기 대기 15.7초 중 settle이 11.65초였다(`docs/test-records/README.md`).
- 워크로드를 0으로 줄이고 원래 수로 되돌리는 어댑터는 이미 있다(`pkg/kube/inventory.go`의 Stop/Restore, decommission이 사용, 0010).

## 결정

Plan에서 고르는 이전 모드를 하나 더 둔다. 기본값은 지금 방식이다.

| 모드 | 대상 앱 | 카나리 | 쓸 앱 |
|---|---|---|---|
| `warm`(기본, 지금 방식) | 전환 전부터 replica 위에서 떠 있다 | 가능 | replica 위에서 기동되는 앱, 또는 기동 시 마이그레이션을 끌 수 있는 앱 |
| `cold`(새로 추가) | 승격 뒤에 띄운다 | 불가 | 기동할 때 스키마를 바꾸며 그것을 끌 수 없는 앱 |

### `cold` 모드의 단계

```
precheck → prepare → presync → delta     대상 앱은 0개. DB만 복제한다
→ fence                                  쓰기를 붙잡는다. 읽기는 원본 앱이 계속 처리한다
→ switchover                             원본 강등, 대상 승격, settle(0009)
→ start-target (새 단계)                 대상 앱을 Plan의 개수로 늘린다
                                         → 모든 파드 ready 대기
                                         → 대상 앱에 직접 스모크 요청
→ cutover                                게이트웨이를 대상으로 돌리고 붙잡은 쓰기를 보낸다
→ verify → finalize
```

- `warm` 모드에서 start-target은 `step_skipped`로 기록한다. 단계 이름과 순서는 고정 목록(0002)에 넣는다.
- start-target이 하는 일은 셋이다.
  1. Plan의 대상 앱 워크로드(Deployment 또는 StatefulSet)를 정한 개수로 늘린다. prepare에서 0개로 만들어 두고 원래 개수를 annotation에 남기는 방식(0010의 Stop/Restore)을 그대로 쓴다.
  2. 모든 파드가 ready가 될 때까지 `startTimeout` 안에서 기다린다. 앱의 기동 마이그레이션은 이제 primary에서 돌므로 성공한다. 양쪽 앱 버전이 같으면 바꿀 것이 없다.
  3. 대상 앱 주소로 Plan에 적은 스모크 요청(조회만)을 보내 기대한 상태 코드를 받는지 본다. 게이트웨이는 아직 원본을 가리키므로 사용자 요청은 대상에 가지 않는다.
- 쓰기 대기는 settle + 앱 기동 + 스모크 시간이 된다. 이 합이 `maxDowntime`을 넘으면 start-target 실패로 본다.

### 원복 경계: start-target 실패는 되돌린다

start-target에서 실패해도 사용자 쓰기는 대상에 하나도 들어가지 않았다. 게이트웨이가 붙잡고 있기 때문이다. 대상에 쓰인 것은 앱 기동이 남긴 스키마 기록 정도다. 그래서 0003의 "승격 요청 뒤에는 멈춘다"를 이 구간에서만 바꾼다.

- 상태에 `trafficOnTarget`이 꺼져 있고 게이트웨이 hold가 살아 있음을 확인한 경우에만 원복한다. 하나라도 확인하지 못하면 지금처럼 `halted`다.
- 원복 순서:
  1. 대상 앱을 다시 0개로 줄인다.
  2. 대상을 강등하고 demotion token을 기다린다.
  3. 그 토큰으로 원본을 다시 승격하고 settle(0009 보완)을 기다린다.
  4. 원본이 대상을 다시 따라가는지(replica 연결) 확인한다.
  5. 게이트웨이 hold를 원본으로 푼다.
- 결과는 `rolled_back`이다. 원복 자체가 어느 단계에서든 실패하면 `halted`다.
- 대상의 스키마 기록(예: 마이그레이션 잠금 행)은 원본이 대상을 따라가는 순간 원본에도 복제된다. 같은 앱 버전이면 내용이 같아서 영향이 없다. 버전이 다르면 이 모드를 쓰지 않는다(아래).

### 쓰지 않는 경우

- 이전하면서 앱 버전을 올리는 경우. 원본 앱과 대상 앱이 다른 스키마를 기대하면 원복 뒤 원본 앱이 새 스키마를 받는다. 이전할 때는 양쪽 앱 버전을 같게 하고, 업그레이드는 이전 전이나 후에 따로 한다. precheck가 양쪽 워크로드의 이미지 태그를 비교해서 다르면 멈추게 할지는 구현할 때 정한다.
- 백그라운드 작업(큐 워커, CronJob)이 게이트웨이를 거치지 않고 DB에 직접 쓰는 앱. 이것은 `warm`과 `cold` 모두의 한계다(fence가 HTTP 쓰기만 붙잡는다). 이 ADR에서 다루지 않는다.

### 계약 변경

- Plan(`pkg/plan`): `target.app`을 추가한다. 필드는 `mode`(`warm`|`cold`), 워크로드 `kind`와 `name`, `replicas`, `startTimeout`, `smokeUrl`, `smoke`(요청 목록, decommission의 smoke와 같은 형식)다. 없으면 `warm`이다.
- platform: `cold` Plan에는 카나리를 허용하지 않는다(시작 요청에 `canary`가 있으면 400). 전환 후 관찰(observe)과 자동 롤백은 그대로 쓴다. 콘솔은 start-target 단계와 모드를 표시한다.
- 권한: 엔진 계정이 대상 네임스페이스의 Deployment/StatefulSet scale을 할 수 있어야 한다(infra의 `hopcharlie-migrator` 범위).

## 검토한 대안

- 앱마다 replica 위에서 뜨게 고치기(moimroom 방식): 우리 앱에는 가장 깔끔하다. 남의 앱은 포크해야 하므로 주제와 맞지 않는다.
- 대상 앱 배포에서만 기동 마이그레이션 끄기: 옵션이 있는 앱이면 `warm` 그대로 되고 카나리도 쓸 수 있다. 가장 먼저 권한다. 다만 옵션이 없는 앱은 해결하지 못한다.
- 대상 앱의 마이그레이션 연결만 원본 primary로 보내기: 대상 DB가 원본을 복제하므로 논리적으로는 같은 DB다. 그러나 대부분의 앱은 DSN이 하나라 나눌 수 없고, 앱 설정을 바꿔야 한다.
- 승격 뒤에 띄우되 start-target 실패는 지금처럼 `halted`: 구현이 작다. 그러나 대상 앱이 안 뜨는 흔한 실패(이미지, 설정, 자원)마다 쓰기가 붙잡힌 채 운영자를 기다려, 0003이 피하려던 상황이 된다.
- cutover 뒤에 대상 앱을 띄우기: 쓰기 대기는 그대로지만 읽기까지 대상으로 가서, 앱이 뜨기 전 요청이 실패한다.

## 결과

- 기동할 때 스키마를 바꾸는 앱도 앱 수정 없이 옮길 수 있다. 대신 그 앱은 전환 전 카나리를 쓰지 못하고, 쓰기 대기가 앱 기동 시간만큼 길어진다. 전환 후 관찰과 자동 롤백이 카나리를 대신한다.
- 원복 경계가 "승격 요청 전"에서 "승격 요청 전, 또는 사용자 쓰기가 대상에 가기 전"으로 넓어진다. 이 새 원복 경로(승격된 대상을 다시 강등)는 지금까지 없던 경로라 실패 지점 테스트를 0003처럼 단계마다 둔다. 적어도 다음을 확인한다.
  - 대상 앱 기동 시간 초과, 스모크 실패, 스케일 요청 실패 → `rolled_back`
  - 원복 중 대상 강등 실패, 원본 재승격 실패, 원본 settle 실패 → `halted`, 쓰기 보류 유지
  - hold가 이미 풀렸거나 `trafficOnTarget`이 켜진 뒤의 실패 → `halted`
- 검증은 기동 마이그레이션을 끌 수 없는 앱(또는 moimroom을 고치기 전 버전)으로 하는 e2e를 platform `test/e2e`에 더해서 한다. 정상 전환, start-target 실패 시 원복 두 시나리오다.
- 구현 전 미확인:
  - 대상 앱이 `cold`로 기동할 때 CNPG settle 재시작과 겹치면 앱 커넥션 풀이 어떻게 되는지. start-target을 settle 뒤에 두는 이유이지만 실제로 재 보지 않았다.
  - 승격된 대상을 다시 강등할 때 CNPG가 정방향 강등(0008)과 같은 시간 안에 끝나는지.
  - 스모크를 게이트웨이를 거치지 않고 대상 앱에 직접 보낼 주소를 Plan에 둘지, 게이트웨이에 "backend 지정 probe"를 둘지.

## 구현 (2026-11-05, 엔진)

이 결정대로 엔진에 구현했다. 구현하면서 정한 세부 사항:

- 단계 이름은 `start-target`, 위치는 switchover와 cutover 사이다. `warm`이면 `step_skipped`로 기록하고 성공으로 친다(재개와 `RunStep` 순서 검사가 그대로 동작).
- `target.app.replicas`는 필수(1 이상)다. prepare의 Stop이 남긴 annotation으로 원래 개수를 되살리는 대신 Plan 값을 쓴다. 대상 앱을 0개로 배포해 둔 경우 annotation 값이 0이라 쓸 수 없기 때문이다. 늘릴 때(`Scale`) Stop annotation을 지운다. 나중의 Stop이 오래된 값을 기록하지 않게 하려는 것이다.
- `cold`는 `gateway.mode: http`일 때만 허용한다. 승격을 되돌려도 되는 근거가 "게이트웨이가 모든 쓰기를 붙잡음"이기 때문이다.
- 스모크는 GET/HEAD만 허용한다. ready 직후 Service가 아직 연결되지 않았을 수 있어서, 모두 통과하거나 시간이 다 될 때까지 반복한다.
- start-target의 제한 시간은 `startTimeout`과 `maxDowntime - 이미 붙잡은 시간` 중 짧은 쪽이다. 이미 넘었으면 앱을 띄우지 않고 실패한다.
- prepare는 앱을 0개로 만든 뒤 파드가 모두 없어질 때까지 `startTimeout` 안에서 기다린다(0011과 같은 이유).
- "hold가 살아 있음"은 게이트웨이에 묻지 않고 엔진 상태 플래그로 확인한다. 게이트웨이 계약(0005)에 hold 조회가 없다. 조건: 실패 단계가 `start-target`, `cold`, `promoteRequested`, `sourceDemoted`, `writesHeld`, `!trafficOnTarget`이다.
- 원복 4단계는 "대상이 원본을 다시 따라가는지"로 구현했다. 원본이 다시 primary가 되므로 위 원복 순서 4번의 "원본이 대상을 따라가는지"는 방향을 거꾸로 적은 것이다. 확인 방법은 복제 지연이 `maxLagBytes` 이하가 되는 것이다(`catchupTimeout`).
- 원복에서 대상 강등이 끝났으면 `promoteRequested`를 끈다. 이후 원본 재승격이 실패해도 결과는 `halted`이고 쓰기는 붙잡힌 채다.

테스트(`pkg/engine/cold_test.go`, 가짜 토폴로지):

- 정상: prepare에서 앱 0개 → 승격 뒤 늘림 → 스모크 → 전환
- start-target 실패 → `rolled_back`: ready 시간 초과, 스모크 실패(500), 스모크 연결 실패, 스케일 거부, 쓰기 보류가 이미 `maxDowntime` 초과(앱을 띄우지 않음)
- 원복 실패 → `halted`, 쓰기 보류 유지: 대상 강등 거부, 대상 강등 토큰 없음, 원복 중 앱 정지 실패, 원본 settle 실패
- start-target 뒤 실패(cutover) → 기존대로 `halted`, 대상 강등 안 함
- 원복 가드를 끄면 원복 테스트와 가드 조건 테스트가 실패하는 것을 확인함

남은 것과 확인하지 않은 것:

- platform: `cold` Plan에 카나리 거부, 콘솔에 `start-target` 단계 표시. 엔진 버전 올림과 함께 한다
- infra: 엔진 계정(`hopcharlie-migrator`)에 대상 네임스페이스 Deployment/StatefulSet patch 권한
- 실제 CNPG에서 승격된 대상을 다시 강등하는 시간과 동작은 재지 않았다. 가짜 토폴로지 테스트만 했다
- 기동 마이그레이션을 끌 수 없는 앱으로 하는 e2e(정상, start-target 실패 시 원복)는 아직 없다
