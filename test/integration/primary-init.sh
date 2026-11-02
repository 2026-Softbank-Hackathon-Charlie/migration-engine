#!/bin/sh
# Allow the standby container to stream WAL from the primary.
echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"
