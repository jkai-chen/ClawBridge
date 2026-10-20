# Hosted deployment options

Status: design candidates, not deployed. The owner suggested a single Compute
Engine VM on 20 October 2026. Local Phase 1 does not provision cloud resources.

## Candidate: one Compute Engine VM

Run `clawbridge serve` and Postgres as separate services on the same VM, with
Postgres reachable only on loopback or a private container network. Put database
data on persistent storage. Keep the Go application independent of Cloud Run
deployment details so both hosting models use the same server and policy checks.

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
3. Direct HTTPS with application validation of Google-issued credentials is
   another design candidate. Validate actual user and service-account flows,
   issuer, audience, signature, expiry and principal mapping before adopting it.
   A development `gcloud` identity token is not evidence that an arbitrary
   production audience/user login flow works.

Before the hosted implementation, run a focused authentication experiment using
the chosen transport. Demonstrate distinct human principals, role denial, token
expiry, and an unattended-agent identity without service-account key files.
IAP tunnel access alone does not complete this experiment.

## Original managed option

Cloud Run plus Cloud SQL remains the source design's original hosting choice.
It reduces database/host maintenance work and provides an IAM-protected HTTPS
entry point. The application must still derive verified caller identity and
enforce ClawBridge project roles; service invocation permission is not project
authorization.

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
