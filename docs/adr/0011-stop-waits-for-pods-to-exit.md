# 0011. decommission의 stop은 종료 중인 파드까지 사라질 때까지 기다린다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

decommission(0010)은 옛 앱을 멈춘(stop) 뒤 관찰(observe)에서 옛 DB 세션을 센다. 남은 세션이 있으면 새 환경이나 다른 곳이 아직 원본에 기대고 있다는 뜻으로 보고 실패한다.

stop이 끝났는지는 워크로드의 `status.replicas`로 판단했다. 그런데 이 값은 종료 신호를 받은 파드를 세지 않는다. 종료 중인 파드는 graceful shutdown이 끝날 때까지 실제로 돌면서 DB 연결을 쥐고 있다. 샘플 앱 moimroom E2E에서 옛 앱 파드가 요청을 마무리하느라 최대 20초 동안 커넥션 풀을 유지했고, 관찰이 이 세션을 "원본 의존성 1개"로 잡아 decommission이 실패했다. 옛 앱 자신의 세션이므로 잘못된 실패다.

## 결정

- Deployment와 StatefulSet의 실행 수는 워크로드의 `spec.selector`에 맞는 파드를 직접 세서 구한다. phase가 `Succeeded`나 `Failed`인 파드만 빼고, 종료 중(deletionTimestamp가 있는) 파드는 센다.
- stop은 이 수가 0이 될 때까지 기다린 뒤 관찰을 시작한다. 기다리는 시간의 상한과 실패 처리는 기존 stop과 같다.
- CronJob은 지금처럼 `status.active`를 본다.

## 검토한 대안

- `status.replicas` 유지하고 관찰 전에 고정 시간 대기: 앱마다 종료 시간이 달라 맞는 값이 없고, 길게 잡으면 모든 decommission이 느려진다.
- 관찰에서 옛 앱의 세션을 빼기: 세션의 출처를 application_name이나 IP로 구분해야 하는데 앱마다 다르고, 옛 앱이 실제로 안 멈춘 경우까지 숨긴다.
- `terminationGracePeriodSeconds`를 줄이기: 앱 설정을 엔진이 바꾸게 되고, 진행 중인 요청을 잘라낸다.

## 결과

- moimroom E2E에서 stop이 6.8초 기다린 뒤 관찰했고, 20초 관찰 동안 옛 DB 세션 0으로 통과했다.
- 파드 목록 권한(`pods` list)이 엔진 계정에 필요하다.
- 셀렉터가 겹치는 다른 워크로드의 파드도 같이 센다. 이 경우 그 워크로드도 멈출 때까지 기다리다 시간 초과로 실패하므로, 조용히 통과하지는 않는다.
