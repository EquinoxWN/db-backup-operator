# ADR 0002: Run WAL-G inside the primary through pod exec, keep credentials out of the operator

- **Status:** Accepted

## Context

WAL-G's `backup-push` reads the PostgreSQL data directory directly, so it must run where that
directory is mounted: in the database pod. PostgreSQL's `archive_command` also calls `wal-g
wal-push` from inside that container, so WAL-G and its storage credentials have to be there anyway.
The operator still needs to decide when a backup runs and to record what happened.

## Decision

The operator does not run backups itself. On schedule it uses the Kubernetes exec API to run
`wal-g backup-push <dataDir>` (and a read-only `psql` query against `pg_stat_archiver`) in the
container that already has WAL-G configured. Commands are passed as an argument list, never through
a shell, and every user-supplied field that reaches a command is restricted by the CRD schema.

## Consequences

- The operator never holds storage credentials; compromising it does not expose backups directly.
- The operator needs `pods/exec`, which is powerful. It is the only write-like permission it has
  besides its own status, and M2 narrows it to namespaced Roles plus an admission policy.
- Backups and WAL archiving are checked through the same path, so one status object shows both.
- Testing the reconciler needs only a fake executor; the real exec path is covered by the M3 kind
  e2e suite.
