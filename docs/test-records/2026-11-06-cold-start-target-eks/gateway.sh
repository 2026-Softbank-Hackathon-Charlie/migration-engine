#!/bin/sh
# gateway.sh <initial-backend>: the platform gateway inside the runner, in front of both app Services.
cd /work
nohup ./gateway -listen 127.0.0.1:8000 -admin-addr 127.0.0.1:9901 \
  -backends src=http://app-src.cold-test.svc:8080,dst=http://app-dst.cold-test.svc:8080 \
  -initial-backend "$1" -state-file /work/gateway-state.json -insecure-no-auth > gateway.log 2>&1 &
sleep 2; echo started
