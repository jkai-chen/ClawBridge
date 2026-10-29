# Deployment assets

- [Deployment guide](../docs/operations/deployment.md): identity, private configuration, persistent disk, HTTPS and application updates.
- [Running the server on your own machine](../docs/operations/local-host.md): `local-host.sh` and `compose.local.yaml`,
  for one machine (`http://127.0.0.1:8443`) or a local network (Caddy on a chosen address), with backups to a local folder.
- [Installing clawbridge-agentd](../docs/operations/agentd.md): the dispatcher agents' launcher, its rootless Docker, units and install check (`systemd/agentd/`, `agentd-check.sh`).
- [Backup and recovery](../docs/operations/backup-and-recovery.md): scheduled dumps, checksums, restore drills and rollback.
- [Historical pilot evidence](../docs/archive/reviews/deployment-pilot.md).

This directory contains the Compose/Caddy templates, systemd units, backup runner
(to a bucket, or with `CLAWBRIDGE_BACKUP_DIR` to a local folder), the local-host script
and bucket lifecycle template. Configure the selected existing environment before
using them. Updating a client binary does not deploy the service or change its data.
