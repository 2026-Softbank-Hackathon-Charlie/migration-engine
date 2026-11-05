#!/bin/sh
# services.sh: the two app instances and the platform gateway, inside the runner pod.
# app-eks writes to EKS hello-src; app-gke writes to GKE hello-gke through the Tailscale proxy.
# This rehearsal runs both apps in this pod; only their databases are in different clouds.
P=$HELLO_PGPASSWORD
cd /work
APP_ENV=eks LISTEN=127.0.0.1:8081 DATABASE_URL="postgres://app:$P@hello-src-rw.hello.svc:5432/app?sslmode=require&connect_timeout=3" nohup ./hello > app-eks.log 2>&1 &
APP_ENV=gke LISTEN=127.0.0.1:8082 DATABASE_URL="postgres://app:$P@db-gke.hopcharlie-db-network.svc.cluster.local:5432/app?sslmode=require&connect_timeout=3" nohup ./hello > app-gke.log 2>&1 &
nohup ./gateway -listen 127.0.0.1:8000 -admin-addr 127.0.0.1:9901 -backends eks=http://127.0.0.1:8081,gke=http://127.0.0.1:8082 \
  -initial-backend "$1" -state-file /work/gateway-state.json -insecure-no-auth > gateway.log 2>&1 &
sleep 3; echo started
