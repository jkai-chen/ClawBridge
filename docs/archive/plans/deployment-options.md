# Hosted deployment options

Status: deployed pilot plus design candidates. The owner fixed the deployment
direction to Docker Compose on one existing Compute Engine VM, replacing the
Cloud Run plus Cloud SQL direction for the Phase 3 deployment. It can become a
team service after shared identity, backup and recovery controls are verified.

## Candidate: one Compute Engine VM

Run `clawbridge serve`, Postgres and Caddy as separate services on the same VM.
Only Caddy publishes TCP 443. The API and Postgres use an internal-only Compose
network. Caddy joins a separate non-internal egress network for ACME/OCSP and
Google signing-key access; the API also joins that egress network to fetch
Google's token-signing certificates. Point DNS at a static external address and
allow public ingress only on TCP 443. Put database data on a persistent attached
disk and retain Caddy state in persistent volumes.

This may reduce baseline cost for a small team, but no saving is established
until region, machine size, disks, backups and network charges are estimated.
The operator owns OS/database updates, off-VM backups, restore verification,
monitoring and recovery. One VM is one shared failure domain for app and database.

## Google identity still works

`gcloud auth login` remains useful, but Compute Engine does not automatically
apply Cloud Run's IAM invoker protection to an arbitrary application port.

1. IAP TCP forwarding can give Google/IAM-authorized clients a private tunnel to
   the service without a VM external IP or an application load balancer.
   Use a port-specific access policy and restrict VM ingress to the IAP path.
   TCP access is a transport permission, not a per-request identity assertion for
   the application. The hosted service still needs verified caller identity for
   project roles and audit attribution. Do not trust a client-supplied email.
2. HTTPS IAP in front of a Compute Engine backend uses a load balancer. It
   provides signed identity assertions that the backend validates, and can suit
   direct HTTPS clients, but the load balancer changes the baseline cost.
3. Direct HTTPS with Caddy and application validation of Google-issued
   credentials is the fixed Phase 3 Compose direction. The Phase 3 browser/device
   OAuth path is the target join flow, but has not yet been accepted against live
   Google clients. It stores only a refresh credential in the OS keychain and
   keeps short-lived ID tokens in memory. Explicit gcloud impersonation and
   authorized-user ADC plus impersonation remain supported operator alternatives.

Before treating the pilot as a shared team service, run a focused authentication
experiment using the chosen transport. Demonstrate distinct human principals,
role denial, token expiry, and an unattended-agent identity without
service-account key files. Phase 2 exercised real Google impersonation and token
verification through the existing tunnel; Phase 3 still has to establish its live
browser/device OAuth client configuration and acceptance. IAP tunnel access alone
does not complete this experiment.

The public unauthenticated `GET /v1/config` endpoint exposes the configured
Desktop-app and device OAuth IDs plus their matching installed-app application
credentials for first-time discovery. Google treats those native-client
credentials as non-confidential application identifiers once the application is
distributed; ClawBridge still keeps them out of logs, errors, server argv,
MCP/installer argv and agent configuration. Token state, principals, scope,
storage addresses, unrelated credential routes and health details stay private.
`/v1/execute` retains Google ID-token verification. ClawBridge session-token
exchange is not part of this deployment slice.

## Deployment decision gate

Present both estimates for the owner's chosen region with VM sizing assumptions,
database storage, backup retention, networking and managed-service costs. Select
the GCP project explicitly. Obtain confirmation of the concrete billed resources
before provisioning. No cloud resources or IAM grants were created for this note.

## Official references

- [IAP TCP forwarding](https://docs.cloud.google.com/iap/docs/using-tcp-forwarding)
- [IAP for Compute Engine](https://docs.cloud.google.com/iap/docs/enabling-compute-howto)
- [Validating signed IAP identity](https://docs.cloud.google.com/iap/docs/signed-headers-howto)
- [Obtaining Google ID tokens](https://docs.cloud.google.com/docs/authentication/get-id-token)
