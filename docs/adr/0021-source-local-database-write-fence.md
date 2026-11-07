# 0021. 소스 접속 차단과 종료 체크포인트로 HTTP 밖의 쓰기를 제한한다

- 상태: 채택 (실험 기능; 운영 활성화 차단)
- 날짜: 2026-11-07

## 상황

HTTP Hold는 배치, 직접 SQL, HTTP 응답 이후 작업을 막지 않는다. HBA와 세션 0건만으로도 충분하지 않다. 이전 HBA로 인증을 시작했지만 세션 집계에 나오지 않는 backend가 나중에 쓸 수 있기 때문이다. 물리 복제되는 role NOLOGIN이나 ALLOW_CONNECTIONS는 대상의 새로운 읽기까지 막는다.

## 결정

직접 엔진 CLI/SDK에서 다음 옵션을 명시한 계획에만 적용한다. 기본 계획은 바꾸지 않는다.

```json
{"sourceWriteFence":{"mode":"cnpg_hba","writerRoles":["app"],"timeout":"10s"}}
```

1. 전용 앱 DB, 소스 단일 CNPG 인스턴스, 이미 준비된 warm target, 영속적인 게이트웨이 소스 읽기 제외 기능을 요구한다. external/관리형 PostgreSQL, DB 없는 앱, cold target, 유지보수 DB와 공유 DB/역할은 거부한다.
2. Source.DSN은 앱 writer와 다른 비관리자 읽기 전용 observer다. pg_read_all_stats, pg_signal_backend, 비교 데이터 SELECT 및 pg_control_system()/pg_control_checkpoint() EXECUTE가 사전에 필요하다. 엔진은 역할이나 권한을 자동 생성하지 않는다. 모든 LOGIN에서 SET ROLE 또는 상속으로 도달하는 NOLOGIN 역할까지 조사한다. 미신고 writer, 관리자 권한을 가질 수 있는 앱 역할, 불확실한 SECURITY DEFINER 접근, prepared transaction과 지원하지 않는 background writer는 Hold 전에 거부한다.
3. 소스와 대상의 SQL TLS 인증서 SHA256을 각각 고정한다. 인증서가 같거나 plaintext fallback이면 거부한다. 재연결은 첫 인증 전에 기존 CA/호스트/custom 검증과 fingerprint 검증을 함께 구성한다. 인증서 rotation은 중단 사유다. observer에는 search_path=pg_catalog를 강제하고 control 함수를 명시적으로 한정해 앱 스키마의 동명 함수가 관측을 가리지 못하게 한다.
4. UID, system ID, 원본 user HBA의 hash, 인증서 hash와 무작위 소유 lease를 먼저 저장한다. LDAP 비밀값이 있을 수 있는 원본 HBA 문장은 상태나 증거에 저장하지 않는다. 게이트웨이 Exclude RPC의 영속 저장 ACK와 소유 lease, 소스별 진행 읽기 0건을 확인한다. 가중치 100%나 전체 읽기 0건은 증명이 아니다. 선택·재시도·fallback·health probe도 제외해야 한다.
5. 소스 CNPG user HBA에만 신고한 LOGIN의 IPv4/IPv6 TCP reject prefix와 소유 annotation을 CAS로 추가한다. 각 역할의 새 연결이 PostgreSQL의 명시적인 HBA reject를 받는지 실제 확인하고 기존 세션과 미완료 트랜잭션을 종료한다. 비밀번호·TLS·소켓 오류는 성공으로 보지 않는다. 이 단계는 초기 접속/세션 정리이며 최종 쓰기 장벽이라고 주장하지 않는다. 대상 HBA는 건드리지 않는다.
6. 최종 장벽은 CNPG 소스 demotion 완료, 고정한 소스 SQL의 recovery=true 및 유효한 실제 system ID다. CNPG 1.30.1이 postmaster 종료 후 발급한 fresh token의 system ID·timeline·0이 아닌 shutdown checkpoint REDO를 확인한다. 소스 pg_control_checkpoint().redo_lsn은 최초에 token과 정확히 같아야 한다. 대상 replay도 token 경계에 도달하고 그 뒤 데이터 hash gate를 통과해야 target promotion을 요청한다. NULL/0/낮은 source replay를 최종 marker로 대신 쓰지 않는다.
7. demotion 중 API status.systemID는 사라질 수 있다. API 값이 있으면 불일치를 거부하며, 없을 때는 UID·단일 인스턴스·replica topology·fresh token 및 고정 소스 SQL에서 직접 읽은 유효한 nonzero system ID를 모두 요구한다. API 부재를 0이나 임의의 값으로 채우지 않는다.
8. HBA의 Active/Released는 초기 접속 통제 상태다. sourceReadOnlyConfirmed와 최종 LSN은 실제 읽기 전용 장벽 확인 뒤에만 저장한다. 그 뒤 소유 HBA와 읽기 제외를 복원해 target 재시작 중 fallback 읽기를 허용한다. HTTP 쓰기는 target 안정화와 route 전환까지 Hold한다. 원본 hash·정확한 prefix·UID·lease가 다르면 타인의 변경을 보존하고 halt한다.
9. demotion 전에는 기존 pinned observer와 source primary가 확인될 때만 보상한다. demotion 요청 이후의 불확실성이나 저장 실패는 HBA의 Released와 관계없이 halt한다. 프로세스 재시작으로 observer를 잃으면 일반 resume로 우회하지 않는다. PromoteRequested 이후 자동 rollback 금지를 유지한다.

## 검토한 대안

- default_transaction_read_only: 일반 세션이 바꿀 수 있고 기존 트랜잭션의 강제 장벽이 아니다.
- NOLOGIN / ALLOW_CONNECTIONS: catalog가 복제되어 target의 새로운 읽기까지 막는다.
- NetworkPolicy만 변경: CNI의 기존 연결 처리와 이미 실행 중인 SQL을 입증하지 못한다.
- HBA와 세션 0건만 사용: 이전 HBA를 선택한 인증 중 backend를 놓친다. 최종 CNPG 종료 체크포인트가 추가로 필요하다.

## 결과

실제 disposable PostgreSQL primary/standby에서 SET ROLE 우회와 pgdata의 INSERT-only writer 거부, privileged role 거부, 새 HBA 연결 거부, 기존 두 트랜잭션 rollback, target 신규 읽기와 원본 HBA 복원을 검증했다. 원격 unit/race/vet/build도 통과했다. API system ID 누락과 SQL ID의 정상/0/불일치, 인증 전 TLS pin과 앱 함수의 control 관측 위장 회귀도 포함한다. 각 결과는 해당 commit의 CI와 함께 읽어야 하며 변경 후 미실행 코드를 통과했다고 해석하지 않는다.

7b04d02의 [실제 CNPG CI](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/actions/runs/42376749488/job/128415791035)는 CNPG 1.30.1, PostgreSQL 18.6, 두 kind 클러스터에서 shutdown checkpoint→target replay→hash gate→promotion→cutover→verify를 모두 통과했다. 원래 HBA를 선택했지만 세션 집계에 없는 두 client의 SCRAM 응답을 보류했다. 하나는 초기 marker 뒤 실제 commit했고 최종 checkpoint 0/5000028이 late commit LSN 0/4062650을 포함했다. target의 해당 행은 1건이며, demotion 뒤 인증을 푼 두 번째 old backend는 쓰지 못했다.

이 fixture의 쓰기 Hold는 약 33초였으며 성능 최적화 성공 근거로 쓰지 않는다. 최신 상태/시간 증거 변경은 해당 commit의 CI를 별도로 확인한다. HTTP 제어 서버는 DB 시험의 계약 fixture이며 실제 proxy 증거는 병합된 platform #135 회귀와 구분한다. 다음 운영 의존성이 끝나기 전에는 Draft를 유지한다.

운영에는 동시 migration controller 사이의 앱/target 배타 소유권, 소유 HBA/lease만 허용하는 admission/RBAC, 임시 필드를 보존하는 Argo CD와 observer 자격증명이 추가로 필요하다. 아직 제공하거나 적용하지 않았다. 기존 운영 정책의 dry-run 거부를 우회하지 않는다. Platform EnginePlan/OpenAPI와 AI는 새 필드를 노출·보존하지 못하므로 운영 UI에서는 지원하지 않는다. 병합된 stable 엔진 모듈로 별도 consumer 계약을 연결해야 하며 unmerged SHA를 pin하지 않는다.

DB Pod의 Unix peer/local root, CNPG reserved 역할, 관리자 break-glass와 privileged extension은 별도 신뢰 경계다. 이를 모두 차단하거나 클라우드 IAM 전체를 증명하는 기능이라고 주장하지 않는다. 운영 SQL, 권한, Secret, 클러스터와 실제 이전은 변경하지 않았다.

근거:
- [PostgreSQL HBA](https://www.postgresql.org/docs/17/auth-pg-hba-conf.html)
- [역할 도달 권한](https://www.postgresql.org/docs/17/role-membership.html)
- [PostgreSQL control 정보](https://www.postgresql.org/docs/17/functions-info.html)
- [인증/초기화 순서](https://github.com/postgres/postgres/blob/REL_17_STABLE/src/backend/utils/init/postinit.c)
- [CNPG 종료 WAL과 token](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/reconciler/replicaclusterswitch/shutdown_wal.go)
- [CNPG token 검증](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/promotiontoken/promotion_token.go)
