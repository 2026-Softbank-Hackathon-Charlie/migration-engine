# 0019. CNPG가 아닌 PostgreSQL 원본은 별도 전환 경로로 옮기고, 승격 뒤에는 되돌리지 않는다

- 상태: 채택
- 날짜: 2026-11-07
- 관련: [0003 원복 경계](0003-rollback-boundary.md), [0004 fence gate](0004-fence-gate-and-verify.md), [0007 포트와 가짜 토폴로지](0007-ports-and-fake-topology-tests.md), [0009 안정 대기](0009-hold-writes-until-new-primary-settles.md), [0012 cold 모드](0012-start-target-app-after-promotion.md), [0013 halt 재개](0013-shorter-write-pause-and-recoverable-halt.md), platform ADR 0042(compose 앱 가져오기)

## 상황

platform ADR 0042는 VM에서 docker compose로 돌던 앱을 우리 클러스터로 무중단 가져온다. 대상 CNPG Cluster는 VM의 PostgreSQL 컨테이너에서 `pg_basebackup`으로 태어나, `externalClusters`의 `legacy` 항목을 `replica.primary`와 `replica.source`로 두고 streaming으로 따라간다. 전환은 엔진이 맡는다.

지금 엔진은 원본도 CNPG Cluster라고 가정한다.

- `pkg/plan`이 원본의 `namespace`, `cluster`를 요구하고, 엔진은 원본 kube API로 Cluster를 읽는다.
- precheck의 `checkTopology`는 양쪽이 서로를 가리키는지 본다.
- switchover는 원본을 강등하고, CNPG가 낸 demotion token으로 대상을 승격한다.
- 원복은 원본을 다시 승격하고, cold 모드의 `rollbackPromoted`는 승격한 대상을 다시 강등한다.
- 전환 뒤 검증은 옛 원본이 새 primary를 따라간다고 보고 누락 행을 센다.

외부 원본에는 이 가운데 어느 것도 없다. 2026-11-07 kind(CNPG 1.30.1, 원본 `postgres:17.11-alpine`, `-bookworm`)에서 확인한 것은 다음과 같다(platform ADR 0042 상황).

- 대상의 `replica.primary`를 자기 이름으로 바꾸면 promotionToken 없이 승격된다. 엔진의 `SetPrimary(self, "")`는 merge patch에 `promotionToken: null`을 보내 그 필드를 지운다(`pkg/cnpg`).
- 원본에서 `ALTER SYSTEM SET default_transaction_read_only = on`, `pg_reload_conf()`, 200ms 대기, 다른 client backend 종료 뒤 `pg_current_wal_flush_lsn()`을 기록하면 대상 replay LSN이 412ms 안에 그 값에 닿았고, 원본이 커밋한 행이 모두 대상에 있었다.
- 승격 전에 `ALTER SYSTEM RESET default_transaction_read_only`와 reload를 하면 원본이 바로 쓰기를 받고 대상은 계속 따라간다.
- musl 원본의 `en_US.utf8` text 인덱스는 glibc 대상에서 깨진다. 대상에서 REINDEX하면 고쳐지지만 정렬 순서는 원본과 다르다.
- 승격 직후 CNPG가 `archive_mode` 때문에 postgres를 한 번 재시작한다(약 0.4초).

리뷰(PR #24)에서 PostgreSQL 17.11로 확인한 fence의 빈틈은 다음과 같다.

- `default_transaction_read_only`는 기본값만 바꾼다. fence 뒤에도 `BEGIN READ WRITE`로 연 트랜잭션은 커밋된다.
- `ALTER ROLE ... SET default_transaction_read_only = off`(또는 DATABASE)가 있으면 그 role의 새 세션은 서버 설정보다 그 값을 따른다.
- `application_name`은 클라이언트가 정하므로, 엔진 이름을 쓴 세션을 종료에서 빼면 그 세션은 살아남아 쓸 수 있다.
- `pg_terminate_backend(pid)`는 신호만 보내고 기다리지 않는다. 그리고 flush LSN은 `synchronous_commit = off`로 커밋했지만 아직 flush되지 않은 기록을 빠뜨린다.

## 결정

### Plan

```json
"source": {"kind": "external", "name": "legacy", "env": "legacy",
           "dsn": "postgres://app:${LEGACY_PG_PASSWORD}@db-legacy-shop:5432/app"},
"target": {"env": "eks", "namespace": "shop", "cluster": "shop-db-eks", "dsn": "...",
           "reindex": ["public.items_name_idx"],
           "app": {"mode": "cold", "kind": "Deployment", "name": "web", "replicas": 1, "...": "..."}},
"gateway": {"mode": "http", "url": "...", "sourceBackend": "legacy"}
```

- `source.kind`는 `cnpg`(기본) 또는 `external`이다. 비어 있으면 `cnpg`로 보되 값을 채우지 않는다. 기존 Plan의 JSON과 상태 파일이 바뀌지 않게 하려는 것이다.
- `external`이면 `dsn`(superuser, `${VAR}`는 지금처럼 프로세스 환경에서 펼친다)과 `name`(대상 `externalClusters`의 원본 이름)이 필수다. `namespace`, `cluster`, `kubeconfig`, `context`는 받지 않는다. 잘못 적은 값이 쓰이는 것처럼 보이지 않게 하려는 것이다. `env`는 지금처럼 필수이고 게이트웨이 `sourceBackend`의 기본값과 부하 정책 범위(0016)에 쓰인다.
- 대상은 CNPG만 된다(`target.kind`는 비우거나 `cnpg`).
- `external`은 `target.app.mode: cold`를 요구한다. 일반 앱은 read-only replica 위에서 뜨지 못하고, 원본이 멈춘 뒤에는 대상 앱을 미리 띄워 둘 이유도 없다. `warm`과 `target.app` 생략은 거부한다. platform의 카나리는 cold Plan에서 이미 막혀 있다(0012).
- `external`은 `verify.fenceGate`를 끌 수 없다. 원본이 승격 뒤 따라오지 않으므로 fence gate가 유일한 DB 비교다.
- `target.reindex`는 `schema.index` 목록이고 `external`일 때만 받는다. 공백이 들어간 이름은 거부한다. 이름은 따옴표로 감싼 식별자로 쓰므로 대소문자를 그대로 본다.
- `decommission`은 `external`에서 받지 않는다. 원본 정리는 platform이 한다(ADR 0042의 6번).
- `allowSourceRewire`([0018](0018-select-source-upstream-before-three-environment-migration.md))는 `external`에서 받지 않는다. 바꿀 원본 CNPG Cluster가 없다.

### 단계

단계 이름과 순서(0002)는 그대로 두고, 원본이 CNPG라고 가정하는 단계만 외부 원본용 함수로 바꾼다(`pkg/engine/external.go`). presync, delta, cutover는 공유한다.

| 단계 | 외부 원본일 때 |
| --- | --- |
| precheck | 원본이 recovery가 아니고 `default_transaction_read_only`가 off다. DSN 계정이 superuser이고 `allow_alter_system`(17 이상)이 on이다. `pg_db_role_setting`에 `default_transaction_read_only` 항목이 없다. 대상(있으면)의 `replica.primary`와 `replica.source`가 `name`이고 `self`가 `name`과 다르다. rclone, 게이트웨이, 대상 앱을 확인한다. `checkTopology`와 강등 토큰은 쓰지 않는다 |
| prepare | 공유 prepare(대상이 healthy이고 replica이며 위 짝 조건을 만족, cold 앱을 0개로) 뒤에 `target.reindex`가 대상에서 모두 인덱스인지 본다(`to_regclass`, relkind `i` 또는 `I`) |
| presync, delta | 기존과 같다. 복제 지연이 `maxLagBytes` 안이어야 하고, delta의 fence gate 시간 측정도 그대로 한다 |
| fence | 게이트웨이가 쓰기를 붙잡는다. 엔진의 원본 연결 풀을 닫는다. 원본을 fence하고 LSN과 다음 xid를 기록한다. 파일을 마지막으로 복사한다 |
| switchover | 대상 replay LSN이 fence LSN 이상이 되기를 기다린다. fence gate를 돌린다. 원본을 다시 읽어 확인한다(아래). 대상의 `replica.primary`를 자기 이름으로 바꾼다(토큰 없음). 쓰기 가능과 안정(0009)을 기다린다 |
| start-target | `target.reindex`를 차례로 `REINDEX INDEX`한 뒤 cold 앱을 띄우고 스모크한다(0012) |
| cutover | 기존과 같다. 게이트웨이를 대상으로 돌리고 보류를 푼다 |
| verify | DB 비교를 하지 않는다. k6 쓰기 기록이 있으면 대상에서 확인하고, 파일은 원본에서 대상 방향으로만 비교한다(`rclone check --one-way`) |
| finalize | 원본 상태(읽기 전용)와 대상 상태를 남기고, 대상이 topology primary인지 본다 |

게이트웨이에 백엔드 목록을 묻는 API는 없다(`pkg/gateway`는 health, hold, release, route뿐이다). 그래서 precheck는 `gateway.sourceBackend`가 게이트웨이에 있는지 확인하지 못한다. 없는 이름이면 원복의 route가 실패해 run이 halted로 남는다. platform이 Plan을 만들 때 온보딩 job이 넣은 `legacy` 백엔드 이름을 그대로 쓴다.

ADR 0042의 순서와 다른 점이 하나 있다. ADR 0042는 승격, REINDEX, 앱 기동, 안정 대기 순으로 적었다. 엔진은 승격, 안정 대기, REINDEX, 앱 기동 순으로 한다. 승격 직후 CNPG 재시작이 REINDEX나 막 뜬 앱의 연결을 끊지 않게 하려는 것이고, 0012의 cold 모드가 이미 이 순서다.

### 원본 fence

`pkg/pg`의 `FenceWrites`가 한다. 원본 어댑터(`ExternalSource` 포트, `pgExternal`)는 호출마다 짧은 연결을 연다.

1. `ALTER SYSTEM SET default_transaction_read_only = on`, `pg_reload_conf()`.
2. 새 연결을 열어 `default_transaction_read_only`가 on으로 보일 때까지 50ms마다 다시 본다(최대 5초). reload는 비동기라 고정 대기보다 이쪽이 정확하다.
3. 자기 세션(`pg_backend_pid()`)을 뺀 모든 client backend를 `pg_terminate_backend(pid, 5000)`으로 끝낸다. 이 함수는 그 세션이 없어질 때까지 기다린다. false가 나온 pid는 다시 찾아 아직 있으면 실패한다(스스로 끝난 세션도 false다). `application_name`으로 빼는 세션은 없다. 엔진 자신의 원본 풀은 fence 전에 닫고, fence gate가 읽기 전용으로 다시 연다.
4. `pg_current_wal_insert_lsn()`과 `pg_snapshot_xmax(pg_current_snapshot())`(다음 xid)을 기록한다. flush 위치가 insert 위치보다 뒤면 `pg_switch_wal()`로 flush한다. 동기 커밋만 WAL을 flush하므로, 커밋이 아닌 기록이나 partial page의 tail은 flush되지 않은 채 남을 수 있고 standby는 flush된 WAL만 받는다. segment 하나를 넘기는 비용이 있지만 fence마다 한 번이다.

role은 바꾸지 않는다. role 변경은 대상에 복제되어 승격 뒤 앱이 로그인하지 못한다. fence 해제(`UnfenceWrites`)는 `ALTER SYSTEM RESET default_transaction_read_only`와 reload다.

### 승격 직전 재확인

fence는 새 트랜잭션의 기본값을 바꿀 뿐이라, 세션이 `BEGIN READ WRITE`나 `SET default_transaction_read_only = off`로 쓰는 것은 막지 못한다. fence 뒤의 쓰기는 대상에 없을 수 있고, 승격 뒤에는 잃는다. 그래서 switchover는 승격 요청 바로 전에 새 연결로 원본을 다시 읽어 다음을 모두 확인한다. 하나라도 틀리면 승격하지 않고 실패하므로 원복된다(쓴 내용은 원본에 그대로 있다).

- `default_transaction_read_only`가 on이다.
- 다음 xid가 fence 때 기록한 값과 같다. 읽기 전용 작업은 xid를 받지 않으므로, 값이 움직였으면 무언가 썼다.
- xid를 가진(쓰기 트랜잭션이 열린) 다른 client 세션이 없다.

리뷰는 세 번째 조건을 "엔진의 현재 세션 말고 client 세션이 하나도 없다"로 제안했다. 그렇게 하지 않았다. VM의 원본 앱은 계속 떠 있고 커넥션 풀이 종료 직후 다시 붙으므로(kind 증명의 writer도 그랬다), 그 조건이면 전환이 거의 항상 실패한다. 다시 붙은 세션은 read-only 기본값으로 읽기만 한다. 그 세션이 쓰려면 xid를 받아야 하므로 앞의 두 조건이 잡는다. 재확인과 승격 요청 사이의 아주 짧은 구간은 남는다.

autovacuum의 ANALYZE도 xid를 받는다. 그러면 쓰기가 없어도 재확인이 실패하고 원복된다. 데이터는 잃지 않으므로 다시 시도한다.

### fence gate 해시

외부 원본과 대상은 C 라이브러리가 달라 text 정렬이 다를 수 있고, 대상의 text 인덱스는 REINDEX 전까지 깨져 있을 수 있다. 지금 해시는 PK(없으면 `t::text`) 순서로 행을 이어 붙이므로, 같은 데이터라도 양쪽 정렬이 다르면 해시가 다르다.

`pkg/verify`에 `CompareTablesBytewise`를 더한다. 정렬 키마다 `::text COLLATE "C"`를 붙인다. 집계는 seq scan으로 하므로 깨진 인덱스를 읽지 않는다. 엔진의 `sqlVerifier`는 외부 원본일 때만 이 함수를 쓴다.

ADR 0042는 collation이 있는 컬럼만 `COLLATE "C"`로 정렬한다고 적었다. 엔진은 모든 키 컬럼을 text로 바꿔 정렬한다. integer PK도 text로 바꿔 정렬하므로 정렬이 조금 느리다(text 비교, 인덱스 순서 못 씀). 그 대신 컬럼 타입을 카탈로그에서 따로 읽지 않고, collation 없는 타입에 `COLLATE`를 붙여 생기는 오류도 없다. 순서는 양쪽이 같기만 하면 되고, text 표현은 엔진 세션 설정(`pg.SessionSettings`)이 양쪽을 같게 한다. 이 비용은 delta의 fence gate 시간 측정(`maxFenceGate`, 0013)에 그대로 들어가므로, 느려서 보류가 길어질 Plan은 보류 전에 걸러진다.

CNPG 경로의 `CompareTables`는 바꾸지 않는다. 바꿔도 일치 여부는 같지만 해시 값이 달라져 기존 증거와 비교할 수 없고, 위의 비용이 같은 이미지끼리 옮기는 CNPG 경로에는 얻는 것 없이 생긴다.

### 원복 경계

- 승격 요청(`promoteRequested`) 전 실패: fence를 요청했으면(`sourceFenced`) 원본 fence를 풀고, 새 세션에서 `default_transaction_read_only`가 off로 보일 때까지 기다린다. 그 뒤 게이트웨이를 원본으로 돌리고 보류를 푼다. 결과는 `rolled_back`이다. fence 해제가 확인되지 않으면 보류를 풀지 않고 `halted`다.
- `sourceFenced`는 fence 요청 전에 켠다. 응답을 잃은 fence도 적용되었을 수 있고, 해제는 적용되지 않은 fence에 해도 해가 없다.
- 승격 요청 뒤 실패: 아무것도 되돌리지 않는다. 원본은 read-only로 남고 쓰기 보류도 유지하며 run은 `halted`다. 게이트웨이 `MaxHold`가 지난 쓰기는 503으로 끝나고 데이터는 잃지 않는다. 원본은 CNPG Cluster가 아니라서 승격한 대상을 따라갈 수 없으므로 대상을 다시 강등하지 않는다. `rollback`과 `rollbackPromoted`는 외부 원본이면 오류(`errNoDemotionRollback`)를 돌려준다.
- 외부 원본의 start-target은 `maxDowntime` 예산을 넘었다고 앱을 띄우지 않고 실패하지 않는다. 이 시점의 실패는 원복이 아니라 halt라서 보류만 길어지기 때문이다. 넘은 시간은 cutover 증거(`withinBudget`)와 경고 로그에 남는다. 보류 예산은 platform이 `checking`에서 미리 막는다(ADR 0042).

### halt 재개

`-resume-halted`(0013, `Engine.ResumeHalted`)는 외부 원본이면 halt 시점에 따라 다르게 한다. 새 CLI 옵션은 두지 않았다. platform이 halted run에 붙일 버튼이 하나로 끝나게 하려는 것이다.

- 승격 요청 전의 halt(프로세스가 fence나 switchover 중에 죽었거나, 원복이 실패함): 원복을 마친다. fence 단계가 `running`이면 보류와 fence가 저장되지 않은 채 적용되었을 수 있으므로 둘 다 걸린 것으로 보고 푼다. 둘 다 반복해도 된다. 성공하면 run은 `rolled_back`이고 `ResumeHalted`는 `ErrResumeRolledBack`을 돌려준다(CLI는 0이 아닌 코드로 끝난다). 원복이 다시 실패하면 halted로 남고 이유를 `rollbackError`에 남긴다.
- 승격 요청 뒤의 halt: 대상이 topology primary이고 쓰기 가능하며 안정되었는가, 원본이 아직 `default_transaction_read_only` on인가를 본다. 원본에 닿지 못하거나 원본이 다시 쓰기를 받으면 재개하지 않는다. 확인되면 switchover를 성공으로 기록하고 남은 단계를 돈다. start-target이 `running`인 채 멈췄으면(프로세스가 죽음) 그 단계를 다시 돈다. REINDEX와 앱 개수 맞추기는 반복해도 된다. cutover가 `running`이면 지금처럼 운영자가 맡는다.

## 검토한 대안

- **CNPG 경로의 단계 함수 안에서 원본 종류로 분기**: 단계마다 분기가 생겨 CNPG 경로를 읽고 바꾸기 어려워진다. 단계 함수를 통째로 바꾸고 공유 단계만 같이 쓰는 쪽이 CNPG 경로를 그대로 둔다.
- **새 단계 이름(예: `fence-source`)**: 상태 파일, `RunStep` 순서 검사, platform 콘솔의 단계 표시가 모두 바뀐다. 같은 이름으로 같은 의미(보류, 승격, 앱 기동)를 하므로 이름을 늘리지 않았다.
- **승격 요청이 적용되지 않았음을 확인하고 원복**: 원본 강등에는 이 확인이 있다. 대상 승격에서는 요청이 늦게 적용되면 원본과 대상이 둘 다 쓰기를 받는다. 이 경로에서는 halt가 더 안전하다.
- **REINDEX를 switchover 안에서**: switchover 뒤 halt를 재개하면 switchover가 성공으로 기록되어 REINDEX를 건너뛴다. start-target에 두면 재개 때 다시 돈다.
- **fence gate 해시를 모든 경로에서 `COLLATE "C"`로**: 위 "fence gate 해시"에 적은 이유로 하지 않았다.
- **엔진 세션을 `application_name`으로 종료에서 빼기**: 처음 구현이 이랬다. 아무 세션이나 같은 이름을 쓸 수 있어서 fence를 피해 갈 수 있다. 엔진 풀을 닫고 모두 끝내는 쪽으로 바꿨다.
- **flush LSN 기록**: kind 증명은 flush LSN으로 맞았지만, 비동기 커밋이 flush 전이면 빠진다. insert LSN과 필요할 때의 segment switch로 바꿨다.
- **role을 `NOLOGIN`으로 막아 `BEGIN READ WRITE`까지 차단**: role 변경이 대상에 복제되어 승격 뒤 앱이 로그인하지 못한다(ADR 0042). xid 재확인으로 잡는다.
- **halted run 원복용 별도 `-abort` 옵션**: 의미는 더 분명하지만 platform이 버튼을 하나 더 붙여야 한다. 승격 전 halt에서 "이어 가기"가 할 수 있는 안전한 일은 원복뿐이라 `-resume-halted`에 넣었다.

## 결과

- 외부 원본 Plan을 platform이 만들어 넘기면 엔진이 ADR 0042의 5번을 한다. CNPG 원본 Plan은 JSON, 상태 파일, 단계 동작, 해시가 이전과 같다. 새 필드(`kind`, `name`, `reindex`, 상태의 `sourceFenced`, `fenceXid`)는 모두 비면 쓰지 않는다. CNPG 쪽 코드 변경은 prepare의 짝 확인을 함수로 뺀 것, halt 재개의 기록 부분을 함수로 뺀 것, 단계 함수를 고르는 한 줄, halt 재개에서 외부 원본 분기를 앞에 둔 것뿐이고 기존 테스트가 그대로 통과한다.
- 테스트(가짜 토폴로지, `pkg/engine/external_test.go`):
  - 정상: 보류, fence, 마지막 파일 복사, fence gate, 토큰 없는 승격, REINDEX 두 개, 앱 기동, 전환, 검증 순서. 원본 CNPG fake는 한 번도 불리지 않고, 원본은 read-only로 남는다
  - 승격 전 실패 후 `rolled_back`(fence 해제, 원본으로 보류 해제): fence 실패, fence는 적용되었지만 응답이 오류, 마지막 파일 복사 실패, fence LSN replay 시간 초과, fence gate 불일치, fence 뒤 xid가 움직임, 쓰기 트랜잭션이 열린 세션
  - 승격 뒤 실패 후 `halted`(해제, 원본 경로, 대상 강등, 앱 정지 없음): 대상이 쓰기 가능해지지 않음, REINDEX 실패, 앱 ready 시간 초과, cutover 경로 실패
  - fence 해제 실패, 해제 뒤에도 read-only 후 `halted`, 보류 유지
  - fence 중 호출자 context 취소: 원복은 자기 context로 돌아 fence를 푼다
  - halt 재개: REINDEX 실패 뒤 재개하면 대상에서 이어 가고 승격은 한 번만 한다. 승격 전 halt는 재개가 원복을 마친다(`ErrResumeRolledBack`). 프로세스가 fence 중에 죽은 run과 start-target 중에 죽은 run도 각각 원복과 재실행으로 끝난다. 원본이 다시 쓰기를 받음, 원본에 닿지 못함, 대상이 primary가 아님이면 거부
  - precheck 실패: 원본 read-only, recovery, 접속 실패, superuser 아님, `allow_alter_system` off, role 단위 override, 대상이 다른 primary나 source를 가리킴. prepare 실패: `target.reindex`가 인덱스가 아님
  - `rollback`, `rollbackPromoted` 오류와 start-target 예산 무시
  - Plan 검증(`pkg/plan/plan_test.go`의 `TestExternalSource`): fence gate 끔, 공백 든 reindex 이름 포함
- 실제 PostgreSQL(`test/integration/run.sh`, primary와 streaming standby):
  - `TestFenceWritesAndUnfence`: role 단위 override가 `Facts`에 나온다. fence 전에 시작한 쓰기 트랜잭션은 커밋되지 않고, 엔진 이름을 쓴 세션도 끝나고, 새 쓰기가 거부된다. `synchronous_commit = off`로 fence 직전에 커밋한 행까지 standby가 돌려받은 LSN에서 모두 가진다. fence 뒤 `BEGIN READ WRITE` 쓰기는 커밋되지만 다음 xid가 움직여 재확인이 잡는다. 해제 뒤 쓰기가 되고, `IsIndex`와 `ReindexIndex`가 동작한다
  - `TestCompareTablesBytewiseIgnoresCollation`: 같은 행을 `"C"`와 `"und-x-icu"` collation PK에 넣으면 `CompareTables`는 불일치, `CompareTablesBytewise`는 일치이고, 값을 바꾸면 불일치를 잡는다
  - fence 테스트가 공유 서버를 잠시 read-only로 만들기 때문에 `run.sh`는 패키지를 하나씩(`-p 1`) 돌린다
- CNPG 승격(토큰 없는 `replica.primary` 변경), 외부 원본에서 태어난 Cluster, 실제 cutover 순서는 platform ADR 0042의 kind 증명(엔진 밖 스크립트)으로 확인했다. 엔진을 붙인 kind 리허설은 차트가 `dbTopology.external`을 그린 뒤 platform e2e에서 한다.
- 남는 것:
  - 승격 직전 재확인과 승격 요청 사이의 짧은 구간에 `BEGIN READ WRITE`로 쓰면 그 쓰기는 잃는다. 게이트웨이를 거치지 않는 쓰기만 이렇게 할 수 있다(ADR 0012와 같은 한계).
  - autovacuum ANALYZE가 재확인을 실패시킬 수 있다(데이터 손실 없음, 원복 후 재시도).
  - `gateway.sourceBackend`가 게이트웨이에 있는지 엔진이 확인하지 못한다(백엔드 목록 API 없음).
  - REINDEX 시간은 보류 안에 들어간다. 엔진은 따로 제한하지 않고 run 문맥으로 돈다. 예산은 platform이 미리 계산한다.
