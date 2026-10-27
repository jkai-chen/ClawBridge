# Deployment assets

- [Deployment guide](../docs/operations/deployment.md): identity, private configuration, persistent disk, HTTPS and application updates.
- [Installing clawbridge-agentd](../docs/operations/agentd.md): the dispatcher agents' launcher, its rootless Docker, units and install check (`systemd/agentd/`, `agentd-check.sh`).
- [Backup and recovery](../docs/operations/backup-and-recovery.md): scheduled dumps, checksums, restore drills and rollback.
- [Historical pilot evidence](../docs/archive/reviews/deployment-pilot.md).

This directory contains the Compose/Caddy templates, systemd units, backup runner
and bucket lifecycle template. Configure the selected existing environment before
using them. Updating a client binary does not deploy the service or change its data.
