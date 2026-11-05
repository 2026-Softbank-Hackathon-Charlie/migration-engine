#!/bin/sh
# round-x.sh <plan> <tag>: writer (a = EKS hello-src, b = GKE hello-gke) for 4m, engine after 20s.
P=$HELLO_PGPASSWORD
cd /work
DSN_A="postgres://app:$P@hello-src-rw.hello.svc:5432/app?sslmode=require&connect_timeout=3" \
DSN_B="postgres://app:$P@db-gke.hopcharlie-db-network.svc.cluster.local:5432/app?sslmode=require&connect_timeout=3" \
  ./writer -every 100ms -for 4m -acked acked-$2.txt > writer-$2.log 2>&1 &
W=$!
sleep 20
echo "engine start $(date +%T)" > engine-$2.log
./hopmigrate run -plan $1 >> engine-$2.log 2>&1
echo "engine exit=$? $(date +%T)" >> engine-$2.log
wait $W
echo DONE >> engine-$2.log
