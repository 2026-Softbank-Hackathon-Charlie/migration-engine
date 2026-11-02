# 0001. DB 이전은 CNPG distributed topology의 demote/promote로 한다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

원본 클러스터의 PostgreSQL을 대상 클러스터로 옮기면서 누락 0건, 쓰기 다운타임 3분 이내를 지켜야 한다. 두 클러스터 모두 CNPG(CloudNativePG)로 DB를 운영한다. 이전하는 동안 원본은 계속 쓰기를 받고, 전환 순간에만 짧게 쓰기를 멈춘다. 전환 뒤에도 쓰기 주체는 항상 한 곳이어야 한다.

## 결정

- CNPG의 distributed topology를 쓴다. 원본과 대상 Cluster 모두 `spec.replica.self/primary/source`를 설정하고, `externalClusters`에 서로를 등록한다. 평소에는 대상이 원본을 스트리밍 복제로 따라간다.
- 전환은 CNPG가 제공하는 두 동작으로만 한다.
  - 원본 demote: 원본의 `spec.replica.primary`를 대상 이름으로 바꾼다. 원본은 읽기 전용이 되고 `status.demotionToken`을 내놓는다.
  - 대상 promote: 대상의 `spec.replica.primary`를 자기 이름으로 바꾸고 `promotionToken`에 그 토큰을 넣는다. 대상은 토큰에 담긴 LSN까지 replay한 뒤에만 승격한다.
- Cluster 변경은 merge patch로 `spec.replica`만 고친다(`pkg/cnpg`). 대상 Cluster가 없으면 Plan의 `target.manifest`를 server-side apply로 만든다.
- 복제 지연은 엔진이 양쪽 DB에 직접 붙어 LSN으로 잰다(`pkg/pg`). CNPG status만 믿지 않는다.

## 검토한 대안

- 논리 복제(publication/subscription): 버전과 확장이 달라도 되지만, 시퀀스와 DDL이 복제되지 않아 전환 때 따로 맞춰야 한다. 끝 위치를 보장하는 장치도 엔진이 직접 만들어야 한다.
- pg_dump/restore: 데이터 크기만큼 쓰기를 멈춰야 해서 3분을 지킬 수 없다.
- 백업(Barman)에서 대상으로 복원한 뒤 WAL만 따라잡기: 가능하지만 오브젝트 스토어와 WAL 아카이브가 먼저 있어야 한다. 아래 결과의 확인 항목에 따라 필요하면 추가한다.

## 결과

- 누락 0건의 근거가 CNPG의 승격 조건(토큰의 LSN까지 replay)에 있다. 엔진은 그 앞에 fence gate(0004)로 한 번 더 확인한다.
- 전환 뒤 옛 원본은 새 primary를 따르는 읽기 전용 replica로 남는다. 그래서 되돌리는 것도 같은 demote/promote로 할 수 있다.
- 인프라와의 약속이 생긴다. 양쪽 Cluster의 topology 설정, `streaming_replica` 인증서와 CA Secret 복사, Argo CD가 `spec.replica`를 되돌리지 않게 하는 설정(`ignoreDifferences`나 이전 중 auto-sync 끄기)이 필요하다. 예시는 `examples/cnpg/`에 있다.
- 로컬 kind의 CNPG 1.25.1에서 WAL 아카이브 없이 스트리밍만으로 demote/promote가 되는 것을 확인했다(`test/rehearsal/run.sh`). 왕복 이전, 원복, 이전 토큰이 남은 상태의 재이전까지 통과했고 쓰기 다운타임은 3.7~14초였다. 한 쿠버네티스 클러스터 안이라 클러스터 사이 네트워크(Tailscale)와 TLS 이름은 검증하지 않았다.
- 대상이 원본의 app 계정 Secret을 써야 한다(`bootstrap.pg_basebackup.secret`). 리허설에서 대상이 따로 만든 비밀번호로 승격하는 순간 app 비밀번호가 바뀌어, 전환 직후 앱 로그인이 실패하는 것을 확인했다.
- ai-sandbox 설계 초안(11장)에는 DB 이전 방식이 아직 팀 결정 항목으로 남아 있다. 엔진은 이 결정을 전제로 만들었으므로 팀에 공유해야 한다.
