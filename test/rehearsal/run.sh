#!/usr/bin/env bash
# Local CNPG rehearsal: two CNPG clusters in one kind cluster, migrated back and
# forth by the real engine. It covers CNPG demote/promote with streaming only
# (no WAL archive) and the rollback path; it does not cover cross-cluster
# networking, the gateway, files or load.
#
# Needs: docker, kind, kubectl. Run from the repository root:
#   test/rehearsal/run.sh            # create kind + CNPG, run all scenarios
#   KEEP=1 test/rehearsal/run.sh     # keep the kind cluster afterwards
set -euo pipefail

CNPG_VERSION=1.25.1
here=test/rehearsal
work="${WORK:-$(mktemp -d)}"
export KUBECONFIG="$work/kubeconfig"   # never touches ~/.kube/config
bin="$work/hopmigrate"
k() { kubectl -n hop "$@"; }

cleanup() { [ -n "${KEEP:-}" ] || kind delete cluster --name hop >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== setup (work dir $work)"
go build -o "$bin" ./cmd/hopmigrate
kind get clusters 2>/dev/null | grep -qx hop || kind create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG"
kind export kubeconfig --name hop --kubeconfig "$KUBECONFIG" >/dev/null
kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s
kubectl create namespace hop --dry-run=client -o yaml | kubectl apply -f - >/dev/null
k delete cluster --all --wait=true >/dev/null
k delete pvc --all --wait=true >/dev/null
k delete secret pg-dst-app --ignore-not-found >/dev/null
k apply -f "$here/nodeports.yaml" -f "$here/source.yaml" >/dev/null
k wait cluster/pg-src --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s
k wait pod/pg-src-1 --for=condition=Ready --timeout=300s
k exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "SET ROLE app;
  CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2), placed_at timestamptz DEFAULT now(), note text);
  CREATE TABLE order_items (order_id bigint, line int, sku text, PRIMARY KEY (order_id, line));
  CREATE TABLE audit (msg text);
  INSERT INTO orders (total, note) SELECT g * 1.5, 'n' || g FROM generate_series(1, 20000) g;
  INSERT INTO order_items SELECT g, l, 'sku-' || l FROM generate_series(1, 5000) g, generate_series(1, 3) l;
  INSERT INTO audit SELECT 'event ' || g FROM generate_series(1, 300) g;"
SRC_PGPASSWORD="$(k get secret pg-src-app -o jsonpath='{.data.password}' | base64 -d)"
export SRC_PGPASSWORD

fail=0
recovery() { k exec "$1-1" -c postgres -- psql -tA -d app -c 'select pg_is_in_recovery()' 2>/dev/null || echo down; }
applogin() { k exec "$1-1" -c postgres -- env PGPASSWORD="$SRC_PGPASSWORD" psql -h localhost -U app -d app -tAc 'select 1' >/dev/null 2>&1 && echo ok || echo fail; }
streaming() { k exec "$1-1" -c postgres -- psql -tA -c 'select status from pg_stat_wal_receiver' 2>/dev/null; }

# expect <primary> <replica>: within 60s the primary is writable, the replica
# streams from it, a write on the primary reaches the replica, app login works.
expect() {
  local p=$1 r=$2 i
  for i in $(seq 1 30); do
    [ "$(recovery "$p")" = f ] && [ "$(recovery "$r")" = t ] && [ "$(streaming "$r")" = streaming ] && break
    sleep 2
  done
  local mark="probe-$RANDOM"
  k exec "$p-1" -c postgres -- psql -q -d app -c "SET ROLE app; INSERT INTO orders (total, note) VALUES (0, '$mark')" >/dev/null 2>&1 || true
  sleep 2
  local seen; seen=$(k exec "$r-1" -c postgres -- psql -tA -d app -c "select count(*) from orders where note = '$mark'" 2>/dev/null || echo 0)
  local line="primary=$p($(recovery "$p")) replica=$r($(recovery "$r"),$(streaming "$r")) write-replicated=$seen applogin=$(applogin "$p")/$(applogin "$r")"
  if [ "$(recovery "$p")" = f ] && [ "$(recovery "$r")" = t ] && [ "$seen" = 1 ] && [ "$(applogin "$p")" = ok ]; then
    echo "   OK   $line"
  else
    echo "   FAIL $line"; fail=1
  fi
}

# scenario <name> <plan> <expected outcome> <primary after> <replica after>
scenario() {
  echo "== $1"
  local out; out=$("$bin" run -plan "$2" 2>&1) || true
  echo "$out" | grep -E '✘|★|\[rollback\]|\[halt\]|downtime' | sed 's/^/   /'
  local run; run=$(echo "$out" | sed -n 's/^run \([^ ]*\) .*/\1/p' | head -1)
  local got; got=$("$bin" show -run "$run" 2>/dev/null | sed -n 's/.*"outcome": *"\([a-z_]*\)".*/\1/p' | head -1)
  if [ "$got" = "$3" ]; then echo "   OK   outcome=$got"; else echo "   FAIL outcome=$got, want $3"; fail=1; fi
  expect "$4" "$5"
}

scenario "1. migrate pg-src -> pg-dst" "$here/plan.json" succeeded pg-dst pg-src

# Independent acknowledgement oracle: retained on the runner, never replicated.
# This deliberately uses a direct DB writer in the disposable rehearsal only;
# it exits before the reverse migration fences writers. No production contexts.
echo "== acknowledged INSERT/UPDATE/DELETE transaction after cutover"
k exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c 'SELECT pg_wal_replay_pause()' >/dev/null
k exec pg-dst-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "SET ROLE app;
 BEGIN;
 INSERT INTO orders(id,total,note) SELECT 100000+g, g, 'ack-'||g FROM generate_series(1,100) g;
 UPDATE orders SET total=total+100, note='ack-update-'||id WHERE id BETWEEN 1 AND 100;
 DELETE FROM orders WHERE id BETWEEN 101 AND 200;
 COMMIT;" >"$work/ack-transaction.out"
# Only reached after psql received a successful COMMIT result. Failure or lost
# response aborts this scenario, never counts as an acknowledged transaction.
printf '%s\n' 'acknowledged transaction: insert 100, update 100, delete 100' >"$work/acknowledged.log"
oracle_sql="SELECT id,total,note FROM orders WHERE id BETWEEN 1 AND 200 OR id BETWEEN 100001 AND 100100 ORDER BY id"
k exec pg-dst-1 -c postgres -- psql -tA -d app -v ON_ERROR_STOP=1 -c "$oracle_sql" >"$work/acknowledged-final-state.txt"
# Prove lag was injected before allowing the old environment to catch up.
lag=$(k exec pg-src-1 -c postgres -- psql -tA -d app -v ON_ERROR_STOP=1 -c 'SELECT pg_is_wal_replay_paused()')
[ "$lag" = t ] || { echo 'FAIL replay was not paused'; exit 1; }
k exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c 'SELECT pg_wal_replay_resume()' >/dev/null
scenario "2. back, but switchover times out right after demotion -> rolled back" "$here/plan-timeout.json" rolled_back pg-dst pg-src
scenario "3. migrate pg-dst -> pg-src" "$here/plan-back.json" succeeded pg-src pg-dst
k exec pg-src-1 -c postgres -- psql -tA -d app -v ON_ERROR_STOP=1 -c "$oracle_sql" >"$work/returned-final-state.txt"
diff -u "$work/acknowledged-final-state.txt" "$work/returned-final-state.txt"
echo '== acknowledged INSERT/UPDATE/DELETE final states preserved after return (PK prevents duplicate rows)'
scenario "4. migrate pg-src -> pg-dst again (old demotion token in status)" "$here/plan.json" succeeded pg-dst pg-src

[ "$fail" = 0 ] && echo "== all scenarios passed" || { echo "== FAILED"; exit 1; }
