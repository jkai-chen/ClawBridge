# Single-VM deployment template

This is a reviewable template, not a provisioned or production-verified deployment.
The application and Postgres run as separate services on one VM. Postgres has no
published host port; the API publishes only on VM loopback. The distroless app runs
as a non-root user with a read-only filesystem and no Linux capabilities.

The pilot deployment uses one `g1-small` Debian VM in `europe-west2-a` with a
10 GiB boot disk. A VM failure stops both services. There is currently no off-VM
backup or snapshot schedule, so the named Docker volume is not a recovery plan.
The default project network also has site-wide ICMP, RDP and broad internal rules;
The previous world-open SSH rule was disabled. Review the inherited ICMP, RDP and
internal rules before treating the pilot as a team service.

## Decisions before provisioning

The owner selects GCP project, region/zone, machine and disk size, backup retention,
and Google identities. Estimate VM, persistent disk, snapshots/off-VM backups,
egress, public IPv4 or NAT if used, and any registry costs before requesting the
explicit confirmation required by DESIGN.md. IAP access does not make these
resources free. No default gcloud project is selected by this template.

Keep the database volume on durable storage and verify restoration from an off-VM
backup. The default named Docker volume is suitable for reviewing the layout; map
it to the selected persistent storage for an actual deployment. Both services share
one VM failure domain. OS, Docker, database patching and recovery are operator work.

## Google identity

The backend validates Google-signed ID tokens for an explicit audience. The client
uses an explicitly selected initiating gcloud account to impersonate an explicitly
selected service account, obtaining a short-lived audience-bound token without a
service-account key file. The audited principal is the service account in the token.
Sharing one service account loses per-person attribution; select separate controlled
identities when attribution must differ. No human email is trusted from a header.

The initiating account needs the relevant Google impersonation permission. Granting
that permission and creating service accounts are separate owner-approved setup
steps. Generic human `gcloud auth print-identity-token` development tokens are
intentionally unsupported for this production path. Direct Google user OAuth and
IAP-signed HTTP assertions are not implemented by this release.

## Prepare configuration manually

Set these in a private, access-restricted environment file outside version control:

```text
POSTGRES_PASSWORD=<random hexadecimal password>
CLAWBRIDGE_AUDIENCE=https://your-chosen-clawbridge-audience
CLAWBRIDGE_BOOTSTRAP_OWNER=owner-identity@your-project.iam.gserviceaccount.com
```

Use a random hexadecimal password so it can be interpolated safely into the
Postgres URL. This file is a deployment secret, not a ClawBridge message. Docker
configuration rendering may include its value; keep rendered config and logs private.

After approved VM setup, the operator can run Docker Compose with that environment
file and `deploy/compose.yaml`. This repository does not create the VM, firewall,
IAM bindings, disks or billing resources automatically. PostgreSQL should stay
private; do not publish its port to make a connection work.

Keep the repository and environment file in a private deployment directory such as
`/opt/clawbridge`. Rebuild and recreate the API after every application commit;
restarting a VM only restarts the existing container image.

## Connect through IAP-mediated SSH

With the template's loopback-only host API, use an IAP-mediated SSH port forward:

```sh
gcloud compute ssh VM_NAME --project PROJECT_ID --zone ZONE --account HUMAN_EMAIL \
  --tunnel-through-iap -- -N -L 127.0.0.1:8080:127.0.0.1:8080
```

Configure ClawBridge's server URL as `http://127.0.0.1:8080` and its audience as
the chosen service-specific audience above. Every application request still needs
its own Google ID token; SSH/IAP access alone is not per-request identity.
Select firewall and SSH/IAP permissions explicitly. Direct IAP TCP forwarding to
the app port would require a different host bind/firewall layout and is not the
default template. Public HTTPS requires a properly configured TLS proxy.

The VM needs an approved egress path for image retrieval and Google signing-key
refresh. Include any NAT/address/registry implications in the cost estimate.

## Verified pilot state

On 20 October 2026, commit `67c8935` was built on the existing
`instance-claw-bridge-dev` VM. Real Google impersonation, token verification,
workspace/project creation, two-session message exchange, authorization denial,
snapshot publication and outage reads were exercised through an IAP-mediated
loopback tunnel. A container restart preserved the Postgres volume and message
state. This does not cover a VM or disk restore, live client model workflows or
team-scale operation.

## Verification and limits

Local real-Postgres verification is run with `integration/run-postgres.sh` and
documented in PHASE-2-REVIEW.md. Google signature verification is tested using
synthetic signed tokens and intercepted Google certificate responses; that does
not prove live IAM, real Google token minting, IAP, VM deployment or backup restore.
Those live checks remain necessary before calling a deployment production ready.
