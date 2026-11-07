# 0021. Source-local admission and a shutdown checkpoint fence

- Status: proposed; draft implementation, not approved for production activation
- Date: 2026-11-07

HTTP Hold cannot stop batch jobs, direct SQL, or work that outlives its HTTP response. A zero-session snapshot also misses a backend still authenticating. Neither weights=100 nor default_transaction_read_only is a hard write barrier.

The optional direct CLI/SDK plan field is:

```json
{"sourceWriteFence":{"mode":"cnpg_hba","writerRoles":["app"],"timeout":"10s"}}
```

Existing plans remain unchanged. The first supported boundary is a dedicated application database on a single-instance CNPG source, a warm target, and a durable gateway source-read exclusion capability. External/managed PostgreSQL and database-less endpoints reject this option. Source.DSN must identify a separately provisioned, non-superuser read-only observer with pg_read_all_stats, pg_signal_backend, SELECT over verification data, and EXECUTE on pg_control_system() and pg_control_checkpoint(). The engine creates no roles or grants.

Before holding traffic, inventory all LOGIN identities and the roles they can SET or inherit, including NOLOGIN roles. Undeclared schema/table/column/sequence/owner or SECURITY DEFINER authority blocks execution. The observer must have no application write authority. Prepared transactions, active administrator clients, unsupported background writers, shared databases/roles, maintenance databases, absent metadata privileges, and unavailable admission dry-run all block execution.

Pin distinct source/target SQL TLS leaf fingerprints while preserving existing TLS verification. Reject plaintext fallbacks and shared certificates: a physical replica shares the system identifier, so it is not enough to identify the observed source after reconnect. Every observer reconnect and authentication probe must retain the peer pin; certificate rotation fails closed. Persist only the public certificate hash.

Persist intent, a random lease, source UID/system identifier and original user-HBA hash before external mutation. Original HBA text may contain LDAP credentials and must never enter state or evidence. The gateway must acknowledge a durable source-specific exclusion and zero source in-flight reads; it must exclude normal selection, retries, fallbacks and health probes. Read weights and global read counts do not prove that property.

CAS-patch only the source CNPG user-HBA prefix and hopcharlie.dev/write-fence-lease annotation. Deny every declared LOGIN over IPv4 and IPv6, attest a fresh explicit PostgreSQL HBA rejection (password/TLS/socket failures are not proof), and terminate old sessions including open transactions. These are early admission/drain measures. Source HBA is not WAL replicated, so new target readers remain admitted.

The final write barrier is CNPG's completed source demotion plus actual source SQL recovery, with source identity rechecked. Read the fresh CNPG 1.30.1 demotion token's shutdown-checkpoint REDO location, verify its system identifier/timeline/nonzero format, and require the pinned source SQL control checkpoint REDO to match that token, and target replay to reach the token boundary. A NULL source replay LSN is never used as a substitute marker. Never substitute an empty, zero or stale source replay position. Run the data hash gate after this barrier and before requesting target promotion. Only after the source is confirmed read-only may owned HBA and gateway read exclusion be restored for fallback reads during the target restart. HTTP writes remain held until the target settles and routing switches.

Before demotion, restore only when the original pinned observer is alive and the source remains primary. A lost observer, process restart, uncertain demotion or failed post-demotion gate halts for explicit reconciliation. Never reopen writes by reconnecting an uncertain source. Preserve the existing PromoteRequested/no-auto-rollback boundary. HBA cleanup requires the same UID, lease, exact prefix and original suffix hash; third-party changes are preserved and halt cleanup.

A privileged administrator, Unix peer/local root in the DB Pod, CNPG fixed reserved roles and privileged extensions are a trust boundary, not application writers this feature claims to fence. Unexpected observed authority fails closed. Concurrent external re-promotion or bypassing the cluster ownership policy is outside the supported controller boundary.

## Verification and activation dependencies

The disposable PostgreSQL fixture exercises explicit new-connection rejection, rollback of old direct and SET ROLE writers, a writer with only INSERT on pgdata.hidden, target read admission, and original HBA restoration. Unit tests cover lease/UID/hash conflicts, intent persistence, restart halt, missing gateway capability and compensation order. They do not prove real CNPG demotion/restart/promotion behavior. A disposable CNPG 1.30.1 kind rehearsal must additionally prove source SQL availability before target promotion and the shutdown-checkpoint/replay ordering. Until that passes, keep the PR draft and the opt-in inactive.

Production also needs exclusive per-app/target ownership across concurrent migration controllers; the source-only lease by itself does not establish target ownership or constrain another controller that ignores it. This remains a release blocker.

Production also needs a narrowly scoped admission/RBAC companion for owned HBA/lease patches and Argo CD preservation of only these temporary fields. They are not delivered or applied by this engine PR. Existing production policy should reject its preflight. No production SQL, roles, secrets, policies, clusters or migrations were changed.

Platform's current EnginePlan/OpenAPI and AI consumers do not expose this field; passing it through those APIs is unsupported and may discard it. Do not advertise the field in the production UI. A separate consumer contract update must follow a merged stable engine module; never pin an unmerged branch SHA.

Sources:
- [PostgreSQL HBA](https://www.postgresql.org/docs/17/auth-pg-hba-conf.html)
- [PostgreSQL role membership](https://www.postgresql.org/docs/17/role-membership.html)
- [PostgreSQL startup/authentication ordering](https://github.com/postgres/postgres/blob/REL_17_STABLE/src/backend/utils/init/postinit.c)
- [CNPG 1.30.1 shutdown WAL/token generation](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/reconciler/replicaclusterswitch/shutdown_wal.go)
- [CNPG 1.30.1 promotion token validation](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/promotiontoken/promotion_token.go)
