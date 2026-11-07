#!/usr/bin/env bash
# Runs the integration tests against a disposable primary + standby pair.
# Usage: test/integration/run.sh [go test flags...]   (needs Docker)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/compose.yaml")
trap '"${compose[@]}" down -v >/dev/null 2>&1 || true' EXIT
"${compose[@]}" up -d --wait
export HOPCHARLIE_IT_PRIMARY_DSN="postgres://postgres:it@127.0.0.1:55432/shop?sslmode=disable"
export HOPCHARLIE_IT_STANDBY_DSN="postgres://postgres:it@127.0.0.1:55433/shop?sslmode=disable"
cd "$here/../.."
# -p 1: the external-source fence test (ADR 0019) makes the shared server
# read-only for a moment, so the packages must not run at the same time.
go test -tags integration -count=1 -p 1 "$@" ./pkg/pg/... ./pkg/verify/...
# Changes the disposable primary HBA after the other privileged fixtures finish.
export HOPCHARLIE_IT_HBA_FIXTURE=disposable-compose
go test -tags integration,writefence -count=1 "$@" -run '^TestSourceHBAWriteFence$' ./pkg/pg/...
