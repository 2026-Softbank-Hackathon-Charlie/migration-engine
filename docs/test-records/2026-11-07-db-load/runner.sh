#!/bin/bash
set -uo pipefail
export GOMODCACHE=/go/pkg/mod GOCACHE=/go/build GOPROXY=file:///windows-mod/cache/download,https://proxy.golang.org GOMAXPROCS=4
cp -a /source /tmp/engine
cd /tmp/engine
go test -p 2 -tags integration -count=1 -v ./pkg/verify > /evidence/verify-postgres.log 2>&1
integration=$?
go test -p 2 -race -count=1 ./... > /evidence/race.log 2>&1
race=$?
go vet ./... > /evidence/vet.log 2>&1
vet=$?
go build ./... > /evidence/build.log 2>&1
build=$?
printf 'integration=%s race=%s vet=%s build=%s\n' "$integration" "$race" "$vet" "$build" > /evidence/status.log
cat /evidence/status.log
if [ "$integration" -ne 0 ] || [ "$race" -ne 0 ] || [ "$vet" -ne 0 ] || [ "$build" -ne 0 ];then exit 1;fi
