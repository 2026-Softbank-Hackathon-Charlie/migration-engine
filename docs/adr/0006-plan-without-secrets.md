# 0006. Plan에는 비밀값을 넣지 않고 환경 변수로 받는다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

Plan은 AI가 만들고, 플랫폼에 저장되고, 승인 화면에 보이고, 리포에 예시로 남는다. 그런데 엔진은 DB 접속 정보와 게이트웨이 토큰이 있어야 실행된다. 파일 복사도 오브젝트 스토리지 자격 증명이 필요하다.

## 결정

- Plan의 `dsn`, `gateway.url`, `gateway.token`에는 `${SRC_PGPASSWORD}` 같은 환경 변수 참조를 쓴다. 엔진이 `Parse`할 때 펼친다.
- 파일 복사는 rclone을 쓰고, 리모트 설정은 `RCLONE_CONFIG_*` 환경 변수로 받는다. Plan에는 `src:shop-uploads` 같은 리모트 이름만 적는다.
- rclone은 인자 배열로 실행하고 셸을 거치지 않는다. 파일 검증은 `rclone check --one-way`로 원본 파일이 대상에 모두 있는지만 본다.
- 엔진은 DSN과 토큰을 오류 메시지나 이벤트에 직접 넣지 않는다. 다만 드라이버(pgx)가 만든 접속 오류에는 호스트와 사용자 이름이 들어갈 수 있다.

## 검토한 대안

- Plan에 비밀값을 직접 넣기: AI 출력, 플랫폼 DB, 승인 화면 어디로든 새어 나간다.
- 엔진이 Kubernetes Secret을 직접 읽기: 엔진에 Secret 읽기 권한이 필요해지고, 리허설 환경마다 RBAC가 달라진다.

## 결과

- 엔진을 부르는 쪽(CLI 사용자나 플랫폼)이 환경 변수를 넣어 줘야 한다. 플랫폼은 허용한 이름(`SRC_PGPASSWORD`, `DST_PGPASSWORD`, `GATEWAY_TOKEN`)만 펼치게 막는다.
- Plan의 kubeconfig 경로, rclone 실행 파일 경로, rclone 플래그는 엔진이 그대로 쓴다. 신뢰할 수 없는 Plan을 받는 경계(플랫폼)에서 막아야 한다.
