#!/bin/bash
set -euo pipefail
export PATH=/tools:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cp -a /source /tmp/rehearsal
cd /tmp/rehearsal
# Connect to the disposable node's NodePorts inside Docker's kind network.
sed -i 's/127.0.0.1:55542/hop-control-plane:30432/g; s/127.0.0.1:55543/hop-control-plane:30433/g' test/rehearsal/plan*.json
export WORK=/tmp/hop-issue19-work
mkdir -p "$WORK"
trap 'cp -a "$WORK"/acknowledged* "$WORK"/returned* /source/docs/test-records/2026-11-06-issue-19/ 2>/dev/null || true' EXIT
bash test/rehearsal/run.sh
