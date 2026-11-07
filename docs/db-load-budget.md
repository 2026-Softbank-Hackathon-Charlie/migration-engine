# 운영 DB 추가 부하 예산

`HOPMIGRATE_LOAD_POLICY=/etc/hopmigrate/db-load-policy.json`을 엔진 실행 서버에 설정한다. 파일은 운영자만 변경하고 읽기 전용으로 마운트한다. 미설정이면 지표 가드는 비활성이다. 설정 오류나 양쪽 endpoint 등록 누락은 실행 생성 시 실패한다. 적용은 새 엔진 실행부터이며, 파일 변경으로 기존 실행의 정책은 바뀌지 않는다.

아래는 구조 설명용 JSON이다. `REPLACE_*`는 실제 환경의 PromQL로 바꿔야 한다. 배포용 지표나 정상 응답을 대신 제공하지 않는다. `sources`에는 **원본과 대상 모두** 실제 Plan의 `env/namespace/cluster` 키로 등록한다.

```json
{
  "prometheusURL": "https://prometheus.internal",
  "tokenEnv": "HOPMIGRATE_PROMETHEUS_TOKEN",
  "pollSeconds": 5,
  "maxWaitSeconds": 60,
  "maxAgeSeconds": 30,
  "requestTimeoutSeconds": 5,
  "batchSize": 256,
  "batchPauseMillis": 20,
  "queryTimeoutMillis": 2000,
  "sources": {
    "source-env/app/source-db": [
      {"id":"cpu","query":"REPLACE_CPU_RATIO","timestampQuery":"REPLACE_CPU_OLDEST_SCRAPE","max":0.7},
      {"id":"ioLatency","query":"REPLACE_IO_SECONDS","timestampQuery":"REPLACE_IO_OLDEST_SCRAPE","max":0.02},
      {"id":"freeDiskBytes","query":"REPLACE_AVAILABLE_BYTES","timestampQuery":"REPLACE_DISK_OLDEST_SCRAPE","min":10737418240,"critical":true},
      {"id":"walBytes","query":"REPLACE_WAL_BYTES","timestampQuery":"REPLACE_WAL_OLDEST_SCRAPE","max":5368709120,"critical":true}
    ]
  }
}
```

각 query와 timestampQuery는 Prometheus instant query API에서 정확히 한 vector 결과를 반환해야 한다. 실제 DB 및 PVC/노드를 식별하는 라벨을 제한한다. CPU는 0~1 비율, IO는 초, 디스크와 WAL은 바이트다. 위 숫자는 예시이며 워크로드의 안전 기준을 측정해서 정한다. 대상은 승격 전 replica일 때도 지표가 있어야 한다.

timestampQuery는 query가 사용한 **가장 오래된 실제 원본 scrape 시각의 Unix 초**를 값으로 반환해야 한다. `time()`이나 파생 rate 결과의 평가 시각은 오래된 원본 자료를 숨길 수 있으므로 쓰지 않는다. 여러 시계열을 집계하면 해당 원본들의 최소 scrape 시각을 사용한다. 누락 자료를 `or vector(0)`으로 정상화하지 않는다. API는 비어 있는 결과, 복수 결과, NaN/Inf, 음수, 미래·오래된 시각, HTTP 실패에 대해 중단한다. 쿼리의 의미와 라벨 정확성은 운영자가 확인해야 한다.

토큰은 JSON 값에 넣지 않고 `tokenEnv`로 지정한 서버 환경 변수에 저장한다. 인증 없는 내부 수집기는 tokenEnv를 생략할 수 있다. 리다이렉트는 따라가지 않으며 요청과 전체 측정에는 제한 시간이 있다. 정책 파일과 Prometheus 주소를 클라이언트 요청으로 받지 않는다.

CPU/IO 초과 시 새 작업은 maxWaitSeconds 동안 대기한다. 진행 중 가드 대상 작업의 초과나 지표 유실은 엔진 작업을 취소한다. freeDiskBytes/WAL 위험은 즉시 중단한다. 상태와 관측값은 기존 metric 이벤트의 `data.loadBudget` 및 단계 증거에 남긴다. 실패·미완료를 검증 성공으로 표시하지 않는다.

쓰기 차단 전 원본·대상을 다시 검사한다. 차단 중에는 지표 대기나 배치 휴식을 넣지 않으며, 정확한 fence count/hash는 설정한 시간 안에 끝나야 한다. promotion 이후 실패하면 자동 롤백하지 않고 halt한다.

범위: pollSeconds 1~60, maxWaitSeconds 1~600, maxAgeSeconds 1~300, requestTimeoutSeconds 1~15, batchSize 1~1000, batchPauseMillis 1~1000, queryTimeoutMillis 10~30000. endpoint마다 필수 네 규칙을 포함해 4~12개 규칙을 허용한다. 추가 규칙으로 연결 수 등의 max/min도 설정할 수 있다.

이 기능은 전체 DB CPU 사용률을 강제로 고정하거나 CNPG의 실행 중 물리 복제를 감속하지 않는다. 복제 포트 분리는 IO 격리가 아니다. 특히 엔진 대기를 취소해도 이미 생성된 replica의 base backup/streaming은 계속될 수 있다. WAL replay를 자동 중지하거나 운영 클러스터를 삭제하지 않는다. PK 검증은 작은 배치지만 전체 source 읽기와 최대 30분 snapshot이 필요하다. 성공 기록 파일은 기존과 같이 메모리에 읽는다. 대형 테이블의 정확한 count/hash 비용도 남는다. 운영 부하 측정과 정책 배포는 별도 검증 후 수행한다.
