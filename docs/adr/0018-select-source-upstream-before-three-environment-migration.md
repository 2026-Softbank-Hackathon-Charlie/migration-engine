# 0018. 명시적으로 승인한 실행의 prepare에서 현재 primary의 다음 복제 원본을 선택한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

등록 앱은 GKE·EKS·onprem 세 환경에 DB를 둘 수 있지만, 한 번의 이전은 여전히 source와 target 두 DB의 제어된 switchover다. 현재 primary의 `spec.replica.source`는 demotion 뒤 따라갈 외부 DB를 가리킨다. 이 값이 EKS인 GKE primary에서 onprem을 선택하면 기존 strict 짝 검사에서 거절된다. 세 번째 DB를 등록했다는 이유로 target의 현재 복제 경로나 primary 역할까지 임의로 바꾸어서는 안 된다.

## 결정

Plan에 선택적 boolean `allowSourceRewire`를 추가한다. 생략·false는 기존 strict 짝 검사를 그대로 사용하며 topology patch가 발생하지 않는다. 플랫폼은 세 환경을 등록한 앱의 승인 대상 계획에만 true를 넣는다. 계획 생성·조회·precheck는 source를 변경하지 않는다.

true인 실행도 target의 `replica.primary`와 `replica.source`가 실제 source의 self를 가리키고, 실제 PostgreSQL이 recovery 상태여야 한다. 다른 peer를 따르는 cascade replica는 거절한다. source는 healthy한 명시적 distributed primary(`primary == self`)이며 PostgreSQL이 recovery 상태가 아니어야 한다. 양쪽 `externalClusters`에 서로의 self 이름이 존재해야 한다. status에는 외부 연결값을 넣지 않고 이름만 읽는다.

prepare는 target healthy·standby 확인 뒤 source의 실제 DB 역할을 다시 확인한다. 변경이 필요하면 source 어댑터의 선택적 `RewireSource` 기능을 사용한다. 미지원 어댑터는 실패한다. CNPG 구현은 UID·resourceVersion·self·primary를 JSON Patch `test`로 확인하고 `spec.replica.source` 한 필드만 변경한다. `primary`, `self`, `promotionToken`, target 리소스는 수정하지 않는다. CAS 충돌·admission 거부·취소·결과 불명은 자동으로 재시도하지 않고 hold 전에 중단한다.

변경 뒤 source/target UID·self·health·DB recovery 역할을 다시 확인하고 기존 strict `checkTopology`를 통과해야 prepare가 성공한다. 이후 fence/demote/promotion-token/promote 순서와 `PromoteRequested` 이후 자동 rollback 금지는 유지한다. `sourceRewire` 증거는 prepare에 이전/선택 이름과 `metadataOnly`를 기록한다.

## 검토한 대안

- strict 짝 검사 제거: target이 잘못된 primary나 제3 DB를 따르는 구성을 허용하므로 제외한다.
- 세 번째 환경 등록 시 현재 primary를 즉시 변경: 승인된 이전 실행 밖에서 운영 topology가 바뀌므로 제외한다.
- target도 자동 reparent: 복제 계보·데이터 일치·세 번째 환경 운영에 영향을 주는 별도 작업이므로 제외한다.
- `replica.source`와 `replica.primary`를 함께 변경: 준비 작업이 demotion을 시작하므로 제외한다.

## 결과

3개 DB 중 선택한 두 DB의 기존 strict switchover를 사용할 수 있고, legacy 계획은 변경되지 않는다. prepare 중 프로세스가 중단되거나 API 요청 결과가 불명일 때 source의 다음 upstream 선택만 남을 수 있다. source 역할을 원복한다는 이유로 무조건 역 patch하지 않는다. 재시도는 관측한 topology에서 시작하고 이미 선택된 값이면 patch하지 않는다. 이 선택은 DB 전체 writer fencing이나 gateway HA를 제공하지 않는다.

로컬 Go 테스트와 Kubernetes dynamic fake의 JSON Patch/CAS 검증은 실제 CNPG 재조정 동작을 증명하지 않는다. 운영 적용 전에 infra의 admission 정책이 현재 primary의 등록된 externalCluster 선택만 허용해야 하고, 격리된 CNPG에서 source 변경 중 writable 유지·target standby 유지·hold 전 실패·실제 switchover를 검증해야 한다. 이 변경에는 운영 patch나 배포를 실행하지 않는다.

근거: [CNPG distributed replica topology](https://cloudnative-pg.io/documentation/current/replica_cluster/)는 primary와 self로 역할을 결정하고 replica.source를 복제 원본으로 구분한다. 사용 중인 CNPG 버전에서 primary 상태의 source 변경 영향은 실제 통합 검증이 필요하다.
