# 0019. 소스 HBA와 세션 종료로 HTTP 밖의 앱 쓰기를 차단한다

- 상태: 제안
- 날짜: 2026-11-07

## 상황

게이트웨이 Hold는 HTTP 요청만 제어한다. 같은 DB에 직접 접속한 배치,
백그라운드 작업, HTTP 응답 이후 계속되는 트랜잭션은 기존 fence LSN 뒤에도
커밋할 수 있다. HTTP 실패율이나 세션 개수만으로 쓰기가 중단됐다고 판단할 수 없다.

## 결정

`sourceWriteFence: {mode: cnpg_hba, writerRoles: [app], timeout: 10s}`를 명시한
계획만 새로운 경계를 사용한다. 기본 계획의 동작은 바꾸지 않는다.

1. 소스는 단일 인스턴스의 전용 앱 DB 클러스터여야 한다. 소스 DSN은 앱 writer와
   다른 읽기 전용 observer다. observer에는 관측과 일반 세션 종료 권한이 필요하며,
   엔진이 역할이나 권한을 자동 생성하지 않는다. 이미 열린 특권 관리자 연결,
   준비된 트랜잭션, 지원하지 않는 background writer는 실행 전에 거부한다.
2. 모든 ordinary LOGIN의 스키마·테이블·열·시퀀스·역할 상속·사용자 SECURITY
   DEFINER 함수 접근을 보수적으로 조사한다. 선언하지 않은 잠재 writer나 observer
   자신의 쓰기 권한을 발견하면 중단한다. 함수 본문을 분석해 안전하다고 추정하지 않는다.
3. UID, 기존 user HBA의 SHA-256, 임의의 소유 lease를 실행 상태에 먼저 저장한다.
   인증 옵션에 LDAP 비밀값이 있을 수 있어 원문 HBA는 상태·증거에 저장하지 않는다.
4. 게이트웨이 Hold 뒤 모든 읽기가 warm target으로 향하고 진행 중 읽기가 0인지
   실제 응답으로 확인한다. 소스 CNPG user HBA 맨 앞에 writer마다 IPv4·IPv6 TCP
   reject 규칙을 넣는다. 사전 server dry-run으로 admission 권한을 확인한다.
5. 각 LOGIN으로 새 TCP 접속을 시도하여 PostgreSQL의 **명시적 HBA reject**를
   관측한다. 암호 오류, TLS 실패, 소켓 오류를 차단 성공으로 취급하지 않는다.
   그 뒤 기존 writer 세션을 `pg_terminate_backend(..., timeout)`으로 종료하고
   세션이 0인지 재확인한 다음에만 fence LSN을 잡는다. 미완료 트랜잭션은 롤백된다.
6. 승격 직전에도 소유 HBA·실제 접속 거부·세션 0을 다시 확인한다. 기존 CNPG
   demotion token과 `PromoteRequested` 경계는 유지한다.
7. 승격 전 실패는 살아 있는 원래 observer와 소스 primary가 확인될 때만 HBA를
   원복한 뒤 HTTP 쓰기를 푼다. observer 유실, 프로세스 재시작, 불확실한 demotion,
   승격 요청 뒤에는 자동으로 소스 쓰기를 열지 않고 halt한다.
8. 정상 승격 뒤에는 소스가 실제 SQL recovery 상태임을 확인하고 fence를 제거한다.
   lease, 정확한 prefix와 나머지 HBA hash가 일치할 때만 CAS로 제거한다. 제3자의
   HBA 변경은 보존하며 자동 덮어쓰기를 거부한다.

## 검토한 대안

- `default_transaction_read_only`: 세션이 변경할 수 있고 기존 트랜잭션까지 막지 못한다.
- role NOLOGIN / 데이터베이스 ALLOW_CONNECTIONS: 카탈로그가 물리 복제돼 target의
  새 읽기 연결까지 막는다. 소스 전용 HBA는 target의 설정·세션을 변경하지 않는다.
- NetworkPolicy만 변경: CNI의 기존 연결 처리에 따라 이미 열린 SQL 연결이 남을 수 있다.

## 결과와 제한

이 경계는 일반 앱 역할의 TCP 접속을 대상으로 한다. DB Pod 내부의 Unix peer,
CNPG 고정 HBA에 선행 허용된 reserved 역할, privileged administrator의 break-glass,
특권 확장 background worker를 전부 차단하는 장치라고 주장하지 않는다. 그런 writer가
관측되거나 ordinary 역할의 권한을 확인할 수 없으면 준비 실패로 처리해야 한다.

다중 인스턴스 소스는 모든 인스턴스의 HBA reload와 failover 후 적용을 증명하는
추가 관측이 필요해 첫 구현에서 거부한다. cold target도 지원하지 않는다. HTTP 밖의
소스 직접 읽기 연결은 writer와 같은 LOGIN을 사용하면 함께 종료된다.

운영 도입에는 명시적인 HBA/lease admission 허용, Argo CD의 해당 임시 필드 보존,
별도 observer 자격정보가 필요하다. 코드나 모의 시험은 운영 환경의 권한·이전 성공을
증명하지 않는다. 실제 PG fixture에서는 소스 접속 거부·기존 TX 롤백·새 target 읽기 유지와
원본 HBA 복구를 검증한다. CNPG 컨트롤러의 실제 reconciliation/admission은 별도 확인한다.

자료:
- [PostgreSQL HBA](https://www.postgresql.org/docs/current/auth-pg-hba-conf.html)
- [PostgreSQL 세션 종료](https://www.postgresql.org/docs/17/functions-admin.html)
- [CNPG PostgreSQL 설정](https://cloudnative-pg.io/documentation/1.20/postgresql_conf/)
