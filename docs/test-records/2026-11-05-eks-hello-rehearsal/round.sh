#!/bin/sh
# round.sh <plan> <tag>: writer load for 3m, engine run starts after 15s.
P=$HELLO_PGPASSWORD
cd /work
DSN_A="postgres://app:$P@hello-src-rw.hello.svc:5432/app?sslmode=require" \
DSN_B="postgres://app:$P@hello-dst-rw.hello.svc:5432/app?sslmode=require" \
  ./writer -every 100ms -for 3m -acked acked-$2.txt > writer-$2.log 2>&1 &
W=$!
sleep 15
echo "engine start $(date +%T)" > engine-$2.log
./hopmigrate run -plan $1 >> engine-$2.log 2>&1
echo "engine exit=$? $(date +%T)" >> engine-$2.log
wait $W
echo DONE >> engine-$2.log
