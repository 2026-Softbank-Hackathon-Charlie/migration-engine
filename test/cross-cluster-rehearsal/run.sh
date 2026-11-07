#!/usr/bin/env bash
# Cross-cluster rehearsal: the real engine migrates a CNPG database between two
# Kubernetes clusters, a "cloud" one and an "on-prem" one, and back. Unlike
# test/rehearsal (both databases in one cluster), replication, certificates and
# Secrets cross a cluster boundary here, as they will between EKS and the
# on-prem k3s (docs/onprem.md, ADR 0014).
#
#   kind hopcloud  : pg-src (primary), NodePort 30432 -> host 127.0.0.1:55562
#   kind hoponprem : pg-dst (standby), NodePort 30433 -> host 127.0.0.1:55563
#   The clusters reach each other's NodePort by node name on the docker
#   network "kind" (hopcloud-control-plane, hoponprem-control-plane), with
#   sslmode=verify-full against those names (certificates.serverAltDNSNames).
#
# Order (the production EKS -> GKE run of 2026-11-05): source, copy its CA,
# replication certificate and app Secret to the other cluster, target, copy the
# target's CA and replication certificate back, patch the source's replica
# topology, then let the engine migrate cloud -> on-prem and on-prem -> cloud.
# Gateway mode none: this is about the database path, not traffic.
#
# Needs docker, kind, kubectl, go. Run from the migration-engine root:
#   test/cross-cluster-rehearsal/run.sh      KEEP=1 keeps both kind clusters
set -euo pipefail

CNPG_VERSION="${CNPG_VERSION:-1.30.1}"
here=test/cross-cluster-rehearsal
work="${WORK:-$(mktemp -d)}"
mkdir -p "$work"
cloud="$work/cloud.kubeconfig"
onprem="$work/onprem.kubeconfig"
kc() { kubectl --kubeconfig "$cloud" -n hop "$@"; }
ko() { kubectl --kubeconfig "$onprem" -n hop "$@"; }
bin="$work/hopmigrate.exe"

cleanup() {
  local fixture_status=$?
  if [ "$fixture_status" != 0 ] && [ "${SOURCE_WRITE_FENCE_REHEARSAL:-}" = 1 ]; then
    # Fixture-only diagnostics, never Secret objects or connection strings.
    for c in cloud onprem; do
      kcfg="$work/$c.kubeconfig"
      [ -f "$kcfg" ] || continue
      kubectl --kubeconfig "$kcfg" -n hop get clusters -o 'custom-columns=NAME:.metadata.name,PHASE:.status.phase,READY:.status.readyInstances,SYSTEM:.status.systemID' || true
      kubectl --kubeconfig "$kcfg" -n hop get pods || true
      kubectl --kubeconfig "$kcfg" -n cnpg-system logs deploy/cnpg-controller-manager --tail=40 || true
      kubectl --kubeconfig "$kcfg" -n hop logs -l cnpg.io/cluster --all-containers --tail=60 --max-log-requests=4 || true
    done
  fi
  [ -n "${KEEP:-}" ] || { kind delete cluster --name hopcloud >/dev/null 2>&1 || true; kind delete cluster --name hoponprem >/dev/null 2>&1 || true; }
}
trap cleanup EXIT

echo "== setup (work dir $work, CNPG $CNPG_VERSION, engine $(git rev-parse --short HEAD))"
go build -o "$bin" ./cmd/hopmigrate
for c in cloud onprem; do
  name=hop$c
  kcfg="$work/$c.kubeconfig"
  image_args=()
  [ -z "${KIND_NODE_IMAGE:-}" ] || image_args=(--image "$KIND_NODE_IMAGE")
  kind get clusters 2>/dev/null | grep -qx "$name" || kind create cluster --config "$here/kind-$c.yaml" --kubeconfig "$kcfg" "${image_args[@]}"
  kind export kubeconfig --name "$name" --kubeconfig "$kcfg" >/dev/null
  kubectl --kubeconfig "$kcfg" apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
  kubectl --kubeconfig "$kcfg" -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s >/dev/null
  kubectl --kubeconfig "$kcfg" create namespace hop --dry-run=client -o yaml | kubectl --kubeconfig "$kcfg" apply -f - >/dev/null
  kubectl --kubeconfig "$kcfg" -n hop delete cluster --all --wait=true >/dev/null
  kubectl --kubeconfig "$kcfg" -n hop delete pvc --all --wait=true >/dev/null
  kubectl --kubeconfig "$kcfg" -n hop delete secret -l hopcharlie.dev/copied=true --ignore-not-found >/dev/null
done

echo "== envcheck (ADR 0014) on both clusters"
"$bin" envcheck -kubeconfig "$cloud" -context kind-hopcloud -namespace hop -cnpg-version "$CNPG_VERSION" | sed 's/^/   /'
"$bin" envcheck -kubeconfig "$onprem" -context kind-hoponprem -namespace hop -cnpg-version "$CNPG_VERSION" | sed 's/^/   /'

retry() { local i; for i in $(seq 1 30); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done; "$@"; }

echo "== source pg-src on hopcloud"
retry kc apply -f "$here/source.yaml"
kc wait cluster/pg-src --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s >/dev/null
kc wait pod/pg-src-1 --for=condition=Ready --timeout=300s >/dev/null
kc exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "SET ROLE app;
  CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2), note text);
  INSERT INTO orders (total, note) SELECT g * 1.5, 'n' || g FROM generate_series(1, 20000) g;"
PGPW="$(kc get secret pg-src-app -o jsonpath='{.data.password}' | base64 -d)"
export PGPW

if [ "${SOURCE_WRITE_FENCE_REHEARSAL:-}" = 1 ]; then
  # Test-only observer, installed before basebackup so target reads use the
  # same dedicated read-only identity. No production endpoint is accepted.
  kc exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "
    CREATE ROLE hop_observer LOGIN PASSWORD 'fixture-observer';
    GRANT pg_read_all_stats, pg_signal_backend TO hop_observer;
    GRANT SELECT ON ALL TABLES IN SCHEMA public TO hop_observer;
    GRANT EXECUTE ON FUNCTION pg_control_system() TO hop_observer;
    ALTER ROLE app SET post_auth_delay='3';"
  kc exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "GRANT EXECUTE ON FUNCTION pg_control_checkpoint() TO hop_observer;"
  # Role settings are read before PostAuthDelay and pgstat_bestart (PG17
  # postinit.c). Delay only the app LOGIN, preserving operator/replication
  # readiness; a server-wide delay would break CNPG's pg_isready timeout.
fi

# copy <from-kubeconfig> <to-kubeconfig> <secret> [key]: copy a Secret (only
# key when given, e.g. a CA's certificate without its private key). Values
# are never printed.
copy() {
  local tmpl
  if [ -n "${4:-}" ]; then
    tmpl="apiVersion: v1
kind: Secret
metadata: {name: {{.metadata.name}}, namespace: hop, labels: {hopcharlie.dev/copied: \"true\"}}
type: Opaque
data:
  $4: {{index .data \"$4\"}}
"
  else
    tmpl='apiVersion: v1
kind: Secret
metadata: {name: {{.metadata.name}}, namespace: hop, labels: {hopcharlie.dev/copied: "true"}}
type: {{.type}}
data:
{{range $k, $v := .data}}  {{$k}}: {{$v}}
{{end}}'
  fi
  kubectl --kubeconfig "$1" -n hop get secret "$3" -o go-template="$tmpl" | kubectl --kubeconfig "$2" apply -f - >/dev/null
  echo "   copied $3${4:+ ($4 only)}"
}

echo "== copy source Secrets to hoponprem"
copy "$cloud" "$onprem" pg-src-ca ca.crt
copy "$cloud" "$onprem" pg-src-replication
copy "$cloud" "$onprem" pg-src-app

echo "== target pg-dst on hoponprem (pg_basebackup across clusters)"
started=$(date +%s)
retry ko apply -f "$here/target.yaml"
target_ready_timeout=600s
[ "${SOURCE_WRITE_FENCE_REHEARSAL:-}" != 1 ] || target_ready_timeout=180s
ko wait cluster/pg-dst --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout="$target_ready_timeout" >/dev/null
ko wait pod/pg-dst-1 --for=condition=Ready --timeout=300s >/dev/null
echo "   healthy after $(( $(date +%s) - started ))s; wal receiver: $(ko exec pg-dst-1 -c postgres -- psql -tA -c 'select status from pg_stat_wal_receiver')"

echo "== copy target Secrets to hopcloud, patch the source's replica topology"
copy "$onprem" "$cloud" pg-dst-ca ca.crt
copy "$onprem" "$cloud" pg-dst-replication
kc patch cluster pg-src --type merge --patch-file "$here/source-patch.json" >/dev/null
kc wait cluster/pg-src --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=300s >/dev/null

if [ "${SOURCE_WRITE_FENCE_REHEARSAL:-}" = 1 ]; then
  export HOPCHARLIE_CNPG_FENCE_FIXTURE=disposable-kind
  export HOPCHARLIE_CNPG_FENCE_WORK="$work"
  go test -tags cnpgfence -count=1 -timeout 15m -run '^TestSourceWriteFenceCNPGShutdownCheckpoint$' -v ./pkg/engine
  exit
fi

plan() { # plan <file> <id> <src> <srcKubeconfig> <srcCtx> <srcPort> <dst> <dstKubeconfig> <dstCtx> <dstPort>
  cat >"$1" <<EOF
{
  "id": "$2",
  "source": {"env": "$3", "kubeconfig": "$4", "context": "$5", "namespace": "hop", "cluster": "pg-$3",
             "dsn": "postgres://app:\${PGPW}@127.0.0.1:$6/app?sslmode=require"},
  "target": {"env": "$7", "kubeconfig": "$8", "context": "$9", "namespace": "hop", "cluster": "pg-$7",
             "dsn": "postgres://app:\${PGPW}@127.0.0.1:${10}/app?sslmode=require"},
  "gateway": {"mode": "none"},
  "verify": {"schemas": ["public"], "fenceGate": true},
  "thresholds": {"maxLagBytes": 1048576, "catchupTimeout": "10m", "switchoverTimeout": "3m", "maxDowntime": "3m"}
}
EOF
}
plan "$work/plan-to-onprem.json" xc-cloud-to-onprem src "$cloud" kind-hopcloud 55562 dst "$onprem" kind-hoponprem 55563
plan "$work/plan-to-cloud.json" xc-onprem-to-cloud dst "$onprem" kind-hoponprem 55563 src "$cloud" kind-hopcloud 55562

fail=0
recovery() { "$1" exec "$2-1" -c postgres -- psql -tA -d app -c 'select pg_is_in_recovery()' 2>/dev/null || echo down; }
streaming() { "$1" exec "$2-1" -c postgres -- psql -tA -c "select status || ' from ' || coalesce(sender_host, '?') from pg_stat_wal_receiver" 2>/dev/null; }

# expect <primary-kubectl> <primary> <replica-kubectl> <replica>: within 60s the
# primary is writable, the replica streams from the other cluster, and a
# write on the primary reaches the replica.
expect() {
  local pk=$1 p=$2 rk=$3 r=$4 i mark seen
  for i in $(seq 1 30); do
    [ "$(recovery "$pk" "$p")" = f ] && [ "$(recovery "$rk" "$r")" = t ] && streaming "$rk" "$r" | grep -q '^streaming' && break
    sleep 2
  done
  mark="probe-$RANDOM"
  "$pk" exec "$p-1" -c postgres -- psql -q -d app -c "SET ROLE app; INSERT INTO orders (total, note) VALUES (0, '$mark')" >/dev/null 2>&1 || true
  sleep 3
  seen=$("$rk" exec "$r-1" -c postgres -- psql -tA -d app -c "select count(*) from orders where note = '$mark'" 2>/dev/null || echo 0)
  local line="primary=$p($(recovery "$pk" "$p")) replica=$r($(recovery "$rk" "$r"), $(streaming "$rk" "$r")) write-replicated-across-clusters=$seen"
  if [ "$(recovery "$pk" "$p")" = f ] && [ "$(recovery "$rk" "$r")" = t ] && [ "$seen" = 1 ]; then echo "   OK   $line"; else echo "   FAIL $line"; fail=1; fi
}

scenario() { # scenario <name> <plan> <expect args...>
  echo "== $1"
  local out; out=$("$bin" run -plan "$2" -state-dir "$work/runs" 2>&1) || true
  echo "$out" >"$work/$(basename "$2" .json).out"
  echo "$out" | grep -E 'writable|settled|downtime|succeeded|rollback|halt|✘' | sed 's/^/   /' || true
  if echo "$out" | grep -q '★ migration succeeded'; then echo "   OK   outcome=succeeded"; else echo "   FAIL migration did not succeed"; fail=1; fi
  shift 2
  expect "$@"
}

scenario "1. cloud -> on-prem" "$work/plan-to-onprem.json" ko pg-dst kc pg-src
scenario "2. on-prem -> cloud" "$work/plan-to-cloud.json" kc pg-src ko pg-dst

[ "$fail" = 0 ] && echo "== all scenarios passed" || { echo "== FAILED"; exit 1; }
