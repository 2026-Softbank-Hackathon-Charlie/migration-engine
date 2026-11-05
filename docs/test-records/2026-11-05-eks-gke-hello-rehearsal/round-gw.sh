#!/bin/sh
# round-gw.sh <plan> <tag>: writes through the gateway for 4m, engine after 20s.
cd /work
./writer -url http://127.0.0.1:8000 -every 100ms -for 4m -acked acked-$2.txt > writer-$2.log 2>&1 &
W=$!
sleep 20
echo "engine start $(date +%T)" > engine-$2.log
./hopmigrate run -plan $1 >> engine-$2.log 2>&1
echo "engine exit=$? $(date +%T)" >> engine-$2.log
wait $W
echo DONE >> engine-$2.log
