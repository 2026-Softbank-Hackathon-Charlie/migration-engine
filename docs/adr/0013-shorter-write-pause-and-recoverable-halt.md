# 0013. 쓰기 정지 시간을 구간별로 재고 줄이며, 안정 확인이 연결 끊김에 흔들리지 않고, 승격 뒤 halt는 엔진이 확인하고 이어 간다

- 상태: 채택
- 날짜: 2026-11-06
- 관련: [0003 원복 경계](0003-rollback-boundary.md), [0009 새 primary 안정 대기](0009-hold-writes-until-new-primary-settles.md), platform ADR 0028(쓰기 재시도 계약)

## 상황

이 엔진의 이전은 쓰기가 멈추지 않는 이전이 아니다. primary가 하나뿐이라 옛 primary의 쓰기를 멈추고 새 primary가 끝까지 따라잡았음을 확인하는 순간이 반드시 있다. 그동안 게이트웨이는 쓰기를 붙잡는다(0005). 요청은 실패하지 않고 데이터도 잃지 않지만, 사용자에게는 쓰기 응답이 그만큼 늦는다. 그래서 목표는 "정지 0"이 아니라 **정지를 짧고 예측 가능하게, 실패 0으로** 두는 것이다.

실측한 정지 시간과 문제는 다음과 같다.

| 실행 | 쓰기 정지 | 비고 |
|---|---|---|
| 로컬 kind, 부하 중 (11/04~11/06) | 8.8~15.1초 | |
| 실제 EKS → GKE hello (11/05, 클러스터 안 러너) | 17.4초 | |
| **운영 moimroom EKS → GKE (11/05, PC 러너)** | **3분 36초** | 엔진 halt. 쓰기 9건 `503 hold_timeout` |

### 정지 시간은 어디에 쓰이나

정지 구간에 엔진이 하는 일을 단계마다 재도록 계측을 넣고(`switchover` 증거 `timingMillis`, `settle`), 로컬 kind(CNPG 1.30.1, PG 18.6, 부하 약 20 req/s)에서 쟀다.

| 구간 | 시간 | 내용 |
|---|---|---|
| fence LSN 따라잡기 | 0.02초 | 보류 전에 지연을 1MiB 아래로 줄여 두므로 짧다 |
| fence gate | 0.02초 | 2만 행 기준. 테이블 크기에 비례한다(아래) |
| 원본 강등 | 1.0초 | CNPG가 원본을 fence, 정지, replica로 재시작하고 토큰을 낸다 |
| 승격 → 쓰기 가능 | 1.0초 | |
| **안정 대기** | **10.96초** | 승격 직후 CNPG의 제자리 재시작 |
| 합계 | 13.1초 | |

안정 대기 동안 엔진이 본 것: `pending_restart` 1회, 서버 재시작 1회, 재시작 중 접속 실패 11회. PostgreSQL 로그로 원인을 확인했다.

1. 승격하면 `archive_mode`가 바뀐다(replica 클러스터와 primary의 값이 다르다). 이 설정은 재시작해야 적용된다(`parameter "archive_mode" cannot be changed without restarting the server`). CNPG가 고정해서 관리하는 값이라 Cluster spec으로 맞출 수 없다. **재시작 자체는 피할 수 없다.**
2. CNPG는 0.2초 뒤 `Restarting primary instance in-place`를 결정하고 smart shutdown을 요청한다.
3. smart shutdown은 클라이언트 세션이 모두 끝날 때까지 `smartShutdownTimeout`(우리 manifest 5초)만큼 기다린 뒤 fast shutdown으로 넘어간다. 대상 앱의 커넥션 풀이 세션을 들고 있으므로 이 5초를 다 쓴다.
4. 재시작 뒤 엔진이 두 번 연속 안정을 확인한다.

### halt가 길어진 운영 사례

11/05 운영 이전에서 대상 DB는 승격 12초 뒤 정상이었다. 그런데 엔진의 대상 DSN이 PC의 `kubectl port-forward`를 거쳤고, 그 연결이 3분 동안 93번 끊겼다. 안정 확인은 읽기 실패를 "안정되지 않음"으로 보고 연속 횟수를 0으로 되돌렸으므로 `switchoverTimeout` 3분 내내 통과하지 못했다. 승격 요청 뒤라 엔진은 설계대로 halt했고(0003), 쓰기는 보류된 채 남았다. 운영자가 DB 상태를 직접 확인하고 게이트웨이를 수동으로 전환할 때까지 쓰기 공백이 3분 36초였다. 엔진은 halt한 run을 재개하지 않으므로 cutover, verify, finalize도 엔진 밖에서 했다.

## 결정

1. **정지 구간을 항상 계측한다.** `switchover` 증거에 `timingMillis`(replayFence, fenceGate, demote, promoteToWritable, settle)와 안정 대기 보고(`settle`: 확인 횟수, 읽지 못한 횟수, 재시작 횟수, CNPG 비정상·recovery·pending_restart 횟수, 마지막으로 막힌 시각)를 남긴다. 로그에도 `target writable`, `target settled` 줄을 찍는다.
2. **CNPG Cluster의 `smartShutdownTimeout`을 1초로 둔다.** 승격 직후 재시작은 피할 수 없으므로 재시작 시간을 줄인다. 이 재시작 동안 쓰기는 게이트웨이에 보류되어 있으므로, 세션을 기다려 줄 이유가 없다. 대상 앱의 세션은 fast shutdown으로 끊기고 커넥션 풀이 다시 붙는다. 실측은 아래 결과에 있다. examples, 리허설 manifest, `docs/details.md`의 권장값을 바꾼다.
3. **안정 확인은 읽기 실패와 재시작을 구분한다.** 읽기 실패(접속 오류, API 오류)는 안정을 깨지도 확인하지도 않는다. 대신 `pg_postmaster_start_time()`을 함께 읽어, 다음에 성공한 읽기의 서버 시작 시각이 바뀌었으면 그사이에 재시작이 있었던 것으로 보고 처음부터 센다. 시작 시각이 같으면 끊긴 동안 재시작은 없었다. 이렇게 하면 끊김이 잦은 경로에서도 실제 재시작은 놓치지 않으면서 안정을 확인한다. 11/05 증상을 재현하는 테스트(`TestSettleToleratesDroppedConnections`)가 바뀌기 전 코드에서는 같은 halt로 실패하고, 바뀐 코드에서는 통과한다.
4. **승격 뒤 halt는 운영자가 엔진에게 이어 가게 한다.** `hopmigrate run -run ID -resume-halted`(`Engine.ResumeHalted`). 엔진이 확인하는 것:
   - 승격 요청 뒤의 halt인가(`PromoteRequested`, `SourceDemoted`). 그 전의 halt는 원복 실패라 사람이 맡는다.
   - 대상 Cluster가 topology primary인가, 대상 DB가 쓰기 가능하고 안정되었는가.
   - 원본 Cluster가 대상을 primary로 가리키고, 원본 DB가 recovery 중인가.

   모두 맞으면 switchover를 성공으로 기록하고(증거 `resumedAfterHalt`), 남은 단계(cutover, verify, finalize)를 그대로 돈다. 하나라도 확인되지 않으면 halted로 남기고 이유를 돌려준다. 자동으로 재개하지는 않는다. 어느 쪽이 primary인지 확인하는 일은 엔진이 하지만, 재개를 시작하는 것은 사람이 정한다.
5. **fence gate는 보류 전에 한 번 재 본다.** fence gate는 비교할 모든 테이블을 행마다 해시하므로 시간이 트래픽이 아니라 데이터 크기에 비례한다. delta 단계(쓰기가 아직 흐를 때)에서 같은 비교를 한 번 돌려 시간을 재고, `thresholds.maxFenceGate`(기본 20초)를 넘으면 쓰기를 보류하기 전에 실패한다. 이때 결과의 불일치는 쓰기가 흐르는 중이라 의미가 없고 시간만 본다. 원본에 전체 스캔이 한 번 더 생기는 비용이 있다.
6. **새 대상의 첫 접속 실패로 run을 끝내지 않는다.** prepare는 대상 Cluster가 healthy가 된 직후 한 번만 접속했다. platform E2E에서 그 한 번이 `tls error: EOF`로 실패해 run이 failed로 끝났다. 이제 `catchupTimeout` 안에서 다시 접속한다(`TestPrepareRetriesTargetConnection`).
7. **대상이 인스턴스 하나면 경고한다.** cutover 뒤 옛 원본으로의 복제는 비동기라, 새 primary가 아직 보내지 못한 마지막 쓰기는 그 노드에만 있다. prepare에서 대상 `instances < 2`면 증거 `warnings`와 로그로 알린다. 데모 클러스터는 일부러 1개라 실패로 하지 않는다.

## 검토한 대안

- 재시작 없애기: `archive_mode`는 CNPG가 고정 관리하는 값이라 spec으로 맞출 수 없다. CNPG를 고치지 않는 한 불가능하다.
- 재시작 전에 쓰기를 내보내기: 0009 이전 방식이고, 내보낸 쓰기가 재시작에 걸려 실패했다(리허설에서 503 8건).
- `smartShutdownTimeout` 0: CNPG에서 0은 기본값(180초)으로 바뀐다고 알려져 있어 쓰지 않는다. 확인하지 않았다.
- 읽기 실패를 그냥 무시하기: 재시작 중에는 접속이 실패하므로 재시작을 놓친다. 서버 시작 시각으로 구분해야 한다.
- halt를 엔진이 스스로 재개하기: 연결이 흔들리는 상황에서 엔진 혼자 확정하면 split-brain 방지 원칙(0003)이 약해진다. 확인은 엔진이, 시작은 사람이 한다.
- 자동 재개 대신 게이트웨이 보류를 자동으로 풀기: 어느 쪽에 풀지 모른다. 0004(platform)에서 기각했다.
- 새 primary를 동기 복제로 옛 원본에 묶기: 옛 원본이 느리거나 끊기면 새 primary의 쓰기도 멈춘다. 클러스터 안 인스턴스 2개 이상(동기 복제)이 더 맞는 해법이다.

## 결과

- 쓰기 정지는 "강등 약 1초 + 승격 약 1초 + CNPG 재시작"으로 설명된다. `smartShutdownTimeout` 1초 실측: 아래 표.
- port-forward처럼 끊김이 잦은 경로에서도 승격이 끝났으면 halt하지 않는다. halt하더라도 운영자는 게이트웨이를 직접 만지지 않고 엔진으로 이어 갈 수 있고, cutover, verify, finalize 기록이 run에 남는다.
- 데이터 크기 때문에 길어질 fence gate는 쓰기를 보류하기 전에 걸러진다.
- 남는 것: 재시작 자체(약 수 초)는 남는다. 플랫폼 콘솔에서 halted run을 재개하는 버튼은 platform이 엔진을 올린 뒤 붙여야 한다(후속). 보류 한도를 넘긴 쓰기의 재시도와 응답이 끊긴 쓰기의 중복은 platform ADR 0028이 다룬다.

### smartShutdownTimeout 실측 (로컬 kind, CNPG 1.30.1, 부하 중)

같은 kind 클러스터에서 5초와 1초를 번갈아 두 번씩 돌렸다(다른 부하 없음, 엔진 이 브랜치, platform 게이트웨이 `fb3eb20`, 부하 약 20 req/s, 쓰기 응답 기록). 원시 로그: `docs/test-records/2026-11-06-write-pause/`.

| 회 | smartShutdownTimeout | 강등 | 승격→쓰기 가능 | 안정 대기 | **정상 이전 쓰기 정지** | 원복 시나리오 보류 | 실패 요청 | 누락 |
|---|---|---|---|---|---|---|---|---|
| 1 | 5초 | 1.0초 | 1.0초 | 11.3초 | **13.4초** | 37.8초 | 0 / 1,211 | 0 |
| 2 | 1초 | 1.0초 | 1.0초 | 6.2초 | **8.4초** | 32.6초 | 0 / 1,247 | 0 |
| 3 | 5초 | 1.0초 | 1.0초 | 10.8초 | **12.9초** | 37.8초 | 0 / 1,174 | 0 |
| 4 | 1초 | 1.1초 | 1.1초 | 6.3초 | **8.4초** | 33.2초 | 0 / 1,213 | 0 |

- 1초로 바꾸면 쓰기 정지가 약 13초에서 8.4초로 줄었다(약 4.8초, 35%). 원복 시나리오의 보류도 같은 만큼 줄었다.
- 남은 안정 대기 약 6초 중 5.2초는 CNPG의 재시작(종료와 기동)이고, 나머지는 두 번 연속 확인하는 폴링이다.
- 같은 조건에서 앞서 다른 kind 클러스터가 동시에 돌 때 잰 1초 실행은 19.3초가 나왔다(강등·승격이 각각 5~6초로 늘어남). CPU 경합으로 오염된 값이라 표에서 뺐다. 실제 클라우드 수치는 아직 재지 않았다.
