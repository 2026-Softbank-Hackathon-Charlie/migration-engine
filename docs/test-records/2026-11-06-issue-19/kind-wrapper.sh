#!/bin/bash
if [ "$1" = create ]; then
 /tools/kind-real "$@" --retain && exit 0
 # Docker Desktop may omit published port metadata even though kubeadm and
 # addons succeeded. Internal export below does not require that metadata.
 docker inspect hop-control-plane >/dev/null 2>&1 || exit 1
 exit 0
fi
if [ "$1" = export ] && [ "$2" = kubeconfig ]; then
 exec /tools/kind-real "$@" --internal
fi
exec /tools/kind-real "$@"
