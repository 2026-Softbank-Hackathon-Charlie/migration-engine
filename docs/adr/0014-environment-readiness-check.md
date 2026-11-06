# 0014. 새 환경은 Plan 전에 envcheck로 이전 대상이 될 수 있는지 확인한다

- 상태: 채택
- 날짜: 2026-11-06
- 관련: [0001 CNPG 분산 토폴로지](0001-cnpg-distributed-topology.md), [0013](0013-shorter-write-pause-and-recoverable-halt.md), platform ADR 0022(동등성 검사)

## 상황

본선 조건은 퍼블릭 클라우드와 온프렘 사이의 이전이다. 지금까지 실제 이전은 EKS와 GKE 사이뿐이고, 플랫폼 환경 목록에는 `onprem`이 이름만 있다. 오늘 온프렘 k3s를 새로 만든다.

엔진의 precheck는 Plan이 있어야 돌고, 원본과 대상 Cluster가 둘 다 있어야 의미가 있다. 그 전 단계에서 클러스터를 만들며 빠뜨리기 쉬운 것들이 있고, 11/05 EKS → GKE 준비에서도 실제로 하나씩 막혔다.

- CNPG operator가 없거나 버전이 다름
- 기본 StorageClass가 없음
- 플랫폼·엔진 자격 증명에 Cluster 수정이나 Secret 권한이 없음
- 엔진이 도는 곳에서 DB 주소에 닿지 않음
- PostgreSQL 메이저 버전이 다름

이것들을 Plan을 만들기 전에, 클러스터를 만드는 사람이 한 명령으로 확인할 수 있어야 한다.

## 결정

1. `hopmigrate envcheck -context C -namespace N`을 둔다(`pkg/envcheck`). 읽기만 하고 아무것도 바꾸지 않는다. 확인 항목:

   | 항목 | fail | warn |
   |---|---|---|
   | kube-api | API에 닿지 않음(나머지 검사 중단) | |
   | namespace | 없음 | |
   | storage-class | StorageClass가 하나도 없음 | 기본값이 없음 |
   | cnpg-crd | CNPG Cluster 리소스가 없음 | |
   | cnpg-operator | 가용 replica 0 | 다른 환경과 minor 버전이 다름, 찾지 못함 |
   | permission | Cluster get·create·patch, Secret get·create, Service get, Pod list가 거부됨 | SubjectAccessReview create 거부(동등성 검사만 영향) |
   | postgres(`-dsn`) | 닿지 않음, 메이저가 `-pg-major`와 다름 | |
   | prometheus·minio(URL) | health가 200이 아님 | |

2. 권한은 지금 쓰는 자격 증명에 SelfSubjectAccessReview로 묻는다. 플랫폼 kubeconfig로 돌리면 플랫폼 권한을, 운영자 kubeconfig로 돌리면 운영자 권한을 본다.
3. 오류 메시지에 DSN 비밀번호를 남기지 않는다.
4. 온프렘 준비 순서와 예시를 함께 둔다: [docs/onprem.md](../onprem.md), `examples/cnpg/target-onprem.yaml`, `examples/cnpg/source-patch-onprem.json`, `examples/plan.eks-to-onprem.json`. 11/05 운영 이전의 모양(Tailscale 프록시 이름, 인증서 SAN, Secret 복사 순서)을 그대로 따른다.

## 검토한 대안

- 엔진 precheck만 쓰기: Plan과 두 Cluster가 있어야 하고, 실패하면 run 하나가 failed로 남는다. 환경을 만드는 중에는 너무 늦다.
- 플랫폼 환경 화면(platform `internal/envs`)에 넣기: 플랫폼이 그 클러스터의 kubeconfig를 가진 뒤에야 볼 수 있다. 온프렘은 그 kubeconfig를 만드는 것이 준비의 일부다. 나중에 같은 검사를 화면에도 보여 줄 수 있다.
- 쓰기 검사(테스트 Cluster를 실제로 만들어 보기): 더 확실하지만 운영 클러스터에 남는 것이 생긴다. 지금은 server dry-run과 권한 질의로 대신한다.

## 결과

- 온프렘을 만드는 동안 무엇이 빠졌는지 단계마다 바로 보인다.
- 통과해도 이전이 된다는 보장은 아니다. 클러스터 사이 복제 경로(인증서, Secret, 서로의 DB 주소)는 Cluster를 만든 뒤 엔진 precheck와 이전에서 확인된다.
