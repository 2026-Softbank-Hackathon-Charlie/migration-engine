# 온프렘을 이전 대상으로 붙이는 순서

온프렘 k3s 클러스터를 EKS·GKE와 같은 이전 대상으로 쓰기 위한 준비 순서다. 2026-11-05 운영 moimroom EKS → GKE 이전([기록](test-records/README.md#2026-11-05-운영-moimroom-실제-이전-aws--gcp))과 같은 모양을 따른다. 각 단계 끝의 `envcheck`가 통과해야 다음으로 간다.

## 0. 정해 둘 것

| 항목 | 이 문서의 가정 | 어디에 쓰이나 |
|---|---|---|
| 플랫폼 환경 이름 | `onprem`, kube 컨텍스트 `onprem-hopcharlie` | infra `terraform/platform/aws/host/envs.yaml` |
| 앱 네임스페이스와 DB | `moimroom`, `moimroom-db-onprem` | infra `apps.yaml` |
| DB 경로 | Tailscale operator 프록시 `db-onprem.hopcharlie-db-network.svc.cluster.local`(EKS의 `db-eks`, GKE의 `db-gke`와 같은 방식, infra ADR 0048) | 대상 manifest, Plan |
| 스토리지 | k3s 기본 `local-path` | 대상 manifest |
| 객체 저장소 | MinIO. 플랫폼 API는 `-tags minio`로 빌드해야 드라이버가 들어간다(platform ADR 0016) | 앱 버킷 |

## 1. 클러스터

1. k3s를 설치하고 kubeconfig를 만든다. 플랫폼과 엔진이 쓸 컨텍스트 이름을 `onprem-hopcharlie`로 둔다.
2. CNPG operator를 EKS·GKE와 같은 버전(1.30.1)으로 설치한다.
3. 앱 네임스페이스 `moimroom`을 만든다.
4. 점검:

```bash
hopmigrate envcheck -context onprem-hopcharlie -namespace moimroom -cnpg-version 1.30.1
```

`kube-api`, `namespace`, `storage-class`, `cnpg-crd`, `cnpg-operator`, `permission *`이 pass여야 한다. `permission create subjectaccessreviews`가 warn이면 이전은 되지만 동등성 검사(platform ADR 0022)의 권한 확인이 unknown으로 남는다.

## 2. 플랫폼 접근

5. 플랫폼용 ServiceAccount와 권한을 만들고 kubeconfig를 SSM `/hopcharlie/kube/onprem`에 넣는다(EKS·GKE와 같은 방식, infra ADR 0036·0044). 이전까지 하려면 `hopcharlie-migrator` 권한(Cluster 생성·수정, Secret 읽기·생성)이 필요하다.
6. 그 kubeconfig로 1번 점검을 다시 돌린다. 이번에는 플랫폼 자격 증명의 권한을 본다.

## 3. DB 경로

7. Tailscale operator와 프록시(`db-onprem`)를 만들고, EKS 쪽에서 `db-onprem`, 온프렘 쪽에서 `db-eks`로 서로의 DB에 닿게 한다. GKE에서는 NodeLocal DNS 때문에 operator의 DNS를 따로 열어야 했다(11/05 기록). k3s도 CoreDNS 구성이 다르면 같은 문제가 날 수 있다.
8. 엔진이 도는 곳에서 두 DB에 닿는지 확인한다.

```bash
MOIMROOM_PGPASSWORD=... hopmigrate envcheck -context onprem-hopcharlie -namespace moimroom \
  -cnpg-version 1.30.1 -pg-major 18 \
  -dsn 'postgres://app:${MOIMROOM_PGPASSWORD}@db-onprem.hopcharlie-db-network.svc.cluster.local:5432/app?sslmode=require'
```

대상 DB가 생기기 전에는 원본 DSN으로 같은 명령을 돌려 경로만 확인한다.

## 4. 대상 DB

9. Secret을 EKS에서 온프렘으로 복사한다. 값은 출력하지 않는다.
   - `moimroom-db-ca`(인증서 `ca.crt`만), `moimroom-db-replication`, `moimroom-db-app`
10. 원본에 대상 프록시 이름의 인증서 SAN을 넣는다(`certificates.serverAltDNSNames: [db-eks...]`). 원본은 재시작하지 않는다.
11. 대상을 만든다: [examples/cnpg/target-onprem.yaml](../examples/cnpg/target-onprem.yaml). pg_basebackup 뒤 streaming lag 0을 확인한다.
12. 대상의 Secret을 온프렘에서 EKS로 복사한다: `moimroom-db-onprem-ca`(`ca.crt`만), `moimroom-db-onprem-replication`.
13. 원본에 `replica`·`externalClusters`를 패치한다: [examples/cnpg/source-patch-onprem.json](../examples/cnpg/source-patch-onprem.json). 원본이 대상을 되돌아 따라갈 수 있게 하는 설정이다(엔진 precheck가 확인).
14. 3번 점검을 대상 DSN으로 돌린다. `postgres`가 `PostgreSQL 18.x, replica`면 준비가 끝났다.

## 5. 이전

15. Plan 예시: [examples/plan.eks-to-onprem.json](../examples/plan.eks-to-onprem.json). 엔진은 클러스터 안에서 돌린다. PC의 port-forward를 거치면 안정 확인이 흔들려 halt할 수 있다(ADR 0013). halt하면 `hopmigrate run -run <id> -resume-halted`로 이어 간다.

## 미리 확인한 것과 남은 것

- `envcheck`는 로컬 kind(CNPG 1.30.1)에서 실제로 돌려 통과와 실패를 모두 확인했다. 대상 manifest는 같은 클러스터에 server dry-run으로 CNPG 스키마를 통과했다. 클러스터 둘 사이 이전 리허설은 `test/cross-cluster-rehearsal`(아래 기록)이다.
- 실제 온프렘 장비, Tailscale 경로, MinIO, 플랫폼 kubeconfig 등록은 아직 없다.
