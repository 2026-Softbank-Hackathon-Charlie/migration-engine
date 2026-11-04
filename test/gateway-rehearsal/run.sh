#!/usr/bin/env bash
# Gateway rehearsal: the real engine migrates a hello app's database while
# load runs through the real platform gateway (gateway.mode http).
#
#   kind (CNPG $CNPG_VERSION): pg-src, pg-dst            namespace hop
#   host: hello-src :55590 -> pg-src, hello-dst :55591 -> pg-dst
#         platform gateway :55580 (proxy) :55581 (admin), backends src, dst
#         load: POST /visit through the gateway, logs every answer
#
# Scenarios
#   1. src -> dst under load                       want succeeded
#   2. dst -> src, switchover times out             want rolled_back
#   3. no 2xx write is missing on the final primary, and requests did not fail
#
# Needs docker, kind, kubectl, go, and the platform repository next to this one
# (PLATFORM_DIR). Run from the migration-engine root:
#   test/gateway-rehearsal/run.sh          KEEP=1 keeps the kind cluster
set -euo pipefail

CNPG_VERSION="${CNPG_VERSION:-1.30.1}"
here=test/gateway-rehearsal
shared=test/rehearsal
platform="${PLATFORM_DIR:-../platform}"
work="${WORK:-$(mktemp -d)}"
mkdir -p "$work"
export KUBECONFIG="$work/kubeconfig"   # never touches ~/.kube/config
k() { kubectl -n hop "$@"; }
pids=()

cleanup() {
  for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  [ -n "${KEEP:-}" ] || kind delete cluster --name hopgw >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== setup (work dir $work, CNPG $CNPG_VERSION)"
go build -o "$work/hopmigrate.exe" ./cmd/hopmigrate
go build -o "$work/rh-hello.exe" ./$here/hello
go build -o "$work/rh-load.exe" ./$here/load
(cd "$platform" && go build -o "$work/rh-gateway.exe" ./cmd/gateway)
echo "   platform gateway from $(cd "$platform" && git rev-parse --short HEAD), engine $(git rev-parse --short HEAD)"

kind get clusters 2>/dev/null | grep -qx hopgw || kind create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG"
kind export kubeconfig --name hopgw --kubeconfig "$KUBECONFIG" >/dev/null
kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s
kubectl create namespace hop --dry-run=client -o yaml | kubectl apply -f - >/dev/null
k delete cluster --all --wait=true >/dev/null
k delete pvc --all --wait=true >/dev/null
k apply -f "$shared/nodeports.yaml" -f "$shared/source.yaml" >/dev/null
k wait cluster/pg-src --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s
k wait pod/pg-src-1 --for=condition=Ready --timeout=300s
echo "   source: $(k exec pg-src-1 -c postgres -- psql -tA -c 'show server_version')"
PGPASS="$(k get secret pg-src-app -o jsonpath='{.data.password}' | base64 -d)"
export SRC_PGPASSWORD="$PGPASS"

rm -f "$work"/*.jsonl "$work"/load.stop "$work"/gateway-state.json "$work"/*-summary.json
DATABASE_URL="postgres://app:$PGPASS@127.0.0.1:55552/app?sslmode=require" "$work/rh-hello.exe" -listen 127.0.0.1:55590 -name hello-src >"$work/hello-src.log" 2>&1 & pids+=($!)
DATABASE_URL="postgres://app:$PGPASS@127.0.0.1:55553/app?sslmode=require" "$work/rh-hello.exe" -listen 127.0.0.1:55591 -name hello-dst >"$work/hello-dst.log" 2>&1 & pids+=($!)
"$work/rh-gateway.exe" -listen 127.0.0.1:55580 -admin-addr 127.0.0.1:55581 -insecure-no-auth \
  -backends src=http://127.0.0.1:55590,dst=http://127.0.0.1:55591 -initial-backend src \
  -state-file "$work/gateway-state.json" >"$work/gateway.log" 2>&1 & pids+=($!)
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:55581/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -fsS -X POST http://127.0.0.1:55580/visit >/dev/null
echo "   app through gateway: $(curl -fsS http://127.0.0.1:55580/)"

plan() { # plan <file> <id> <src> <dst> <srcPort> <dstPort> <switchoverTimeout>
  cat >"$1" <<EOF
{
  "id": "$2",
  "source": {"env": "$3", "context": "kind-hopgw", "namespace": "hop", "cluster": "pg-$3",
             "dsn": "postgres://app:\${SRC_PGPASSWORD}@127.0.0.1:$5/app?sslmode=require"},
  "target": {"env": "$4", "context": "kind-hopgw", "namespace": "hop", "cluster": "pg-$4",
             "dsn": "postgres://app:\${SRC_PGPASSWORD}@127.0.0.1:$6/app?sslmode=require",
             "manifest": "$shared/target.yaml"},
  "gateway": {"mode": "http", "url": "http://127.0.0.1:55581", "sourceBackend": "$3", "targetBackend": "$4"},
  "verify": {"schemas": ["public"], "fenceGate": true, "writtenRecords": "$work/written.jsonl"},
  "thresholds": {"maxLagBytes": 1048576, "catchupTimeout": "10m", "switchoverTimeout": "$7", "maxDowntime": "3m"}
}
EOF
}
plan "$work/plan-1.json" gw-src-to-dst src dst 55552 55553 3m
plan "$work/plan-2.json" gw-dst-to-src-timeout dst src 55553 55552 300ms

"$work/rh-load.exe" -url http://127.0.0.1:55580 -workers 2 -every 100ms \
  -written "$work/written.jsonl" -log "$work/requests.jsonl" -stop "$work/load.stop" \
  -summary "$work/load-summary.json" >"$work/load.log" 2>&1 & loadpid=$!; pids+=($loadpid)
sleep 10

fail=0
scenario() { # scenario <name> <plan> <want>
  echo "== $1"
  local out; out=$("$work/hopmigrate.exe" run -plan "$2" 2>&1) || true
  echo "$out" >"$work/$(basename "$2" .json).out"
  echo "$out" | grep -E 'rollback|halt|downtime|hold|route|release|written|succeeded|failed' | sed 's/^/   /' || true
  local run; run=$(echo "$out" | sed -n 's/^run \([^ ]*\) .*/\1/p' | head -1)
  local got; got=$("$work/hopmigrate.exe" show -run "$run" 2>/dev/null | sed -n 's/.*"outcome": *"\([a-z_]*\)".*/\1/p' | head -1)
  if [ "$got" = "$3" ]; then echo "   OK   outcome=$got"; else echo "   FAIL outcome=$got, want $3"; fail=1; fi
  echo "   gateway state: $(curl -fsS http://127.0.0.1:55581/v1/state)"
  echo "   app: $(curl -fsS http://127.0.0.1:55580/ || echo unreachable)"
}

scenario "1. src -> dst under load" "$work/plan-1.json" succeeded
sleep 10
scenario "2. dst -> src, switchover times out -> rolled back" "$work/plan-2.json" rolled_back
sleep 10

echo "== 3. writes"
touch "$work/load.stop"; wait "$loadpid" || true
sed 's/^/   /' "$work/load-summary.json"
primary=pg-dst
written=$(wc -l <"$work/written.jsonl" | tr -d ' ')
ids=$(sed -n 's/.*"pk":"\([0-9]*\)".*/\1/p' "$work/written.jsonl" | paste -sd, -)
missing=$(echo "SELECT count(*) FROM unnest('{$ids}'::bigint[]) AS w(id) WHERE NOT EXISTS (SELECT 1 FROM visits v WHERE v.id = w.id);" \
  | k exec -i "$primary-1" -c postgres -- psql -tA -q -d app | tail -1)
rows=$(k exec "$primary-1" -c postgres -- psql -tA -d app -c "SELECT count(*) FROM visits" | tail -1)
failed=$(sed -n 's/.*"failed": *\([0-9]*\).*/\1/p' "$work/load-summary.json")
echo "   primary=$primary visits rows=$rows, 2xx answers=$written, missing on primary=$missing, failed requests=$failed"
if [ "$missing" = 0 ] && [ "$written" -gt 0 ]; then echo "   OK   no acknowledged write is missing"; else echo "   FAIL acknowledged writes missing"; fail=1; fi
if [ "$failed" = 0 ]; then echo "   OK   no failed request"; else echo "   WARN $failed requests failed (see requests.jsonl)"; fi

[ "$fail" = 0 ] && echo "== all scenarios passed" || { echo "== FAILED"; exit 1; }
