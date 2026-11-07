# platform #87 파일 가드 최초 검사 검증 원본

이 디렉터리는 파일 가드 개선 과정에서 workspace의 migration-engine 저장소 아래에 수집한 검증 원본이다. 엔진 구현 변경이 아니라 platform 구현의 증거이며, 관련 구현과 최종 요약은 [platform PR #100](https://github.com/2026-Softbank-Hackathon-Charlie/platform/pull/100)에 있다. 구현 저장소의 ADR 0034와 `docs/test-records/2026-11-06-platform-87/README.md`에 실행 방법과 한계를 기록했다.

## 최종 증거

- baseline-minio.log / improved-minio.log: 같은 로컬 MinIO의 각 데이터셋 3회 비교. 작은 텍스트 32개는 중앙값 12.839초 → 2.460초, 전체 프로세스 35 → 5, 다운로드 프로세스 32 → 1이다. 원격 요청은 230 → 233으로 늘었다.
- full-race.log / full-race-status.log: 격리 Linux 사본의 전체 race 테스트 exit 0.
- related-race.log: fileguard와 preflight 관련 race 테스트 통과.
- rclone-integration.log: 실제 rclone 로컬 통합 테스트 통과.
- staging-enospc.log: 전용 1 MiB tmpfs에서 실제 용량 부족 시 승인·업로드 차단과 staging 정리 테스트 통과.
- vet-build.log: vet/build 모두 exit 0.

## 이전 측정과 제약

baseline-local.log / improved-local.log는 로컬 파일 backend 측정이다. baseline-minio-earlier.log와 improved-minio-contended.log는 이전 MinIO 측정이며 위 최종 비교와 섞어 계산하지 않는다. 이력과 측정 환경 차이를 보존하기 위해 함께 남긴다.

검사 이력은 비어 있지만 OS/backend cache는 flush하지 않았다. RSS는 parent와 최대 개별 child를 각각 측정하며 전체 동시 RSS가 아니다. staging peak는 10ms sampling이다. 같은 크기 원본 변경의 완전한 감지, 실제 AWS S3/GCS, 운영 migration과 cross-environment 복구를 검증한 기록은 아니다. 정상 경로의 정책 승인과 검사한 staging의 재검사는 유지한다.

2026-11-07 남아 있던 전용 hop87-minio를 중지했으며 컨테이너와 볼륨은 보존했다. 이번 문서 PR에서는 테스트를 다시 실행하거나 운영 리소스를 변경하지 않았다. 로그는 리뷰를 위해 BOM 없는 UTF-8로 정규화했다.
