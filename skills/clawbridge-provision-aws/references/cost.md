# Monthly cost estimate (AWS)

An **estimate** in US dollars for one server in `us-east-1`, reviewed each
release. Other regions cost up to about 25% more. Prices change over time; the
user's billing console is the authority. Show the user the rows for their chosen
instance size, with the total.

| Item | t4g.small (2 vCPU burstable, 2 GiB) | t4g.medium (2 vCPU burstable, 4 GiB) |
|---|---|---|
| Instance, on demand, 730 h | 12.26 | 24.53 |
| Root volume, 20 GB gp3 | 1.60 | 1.60 |
| Data volume, 20 GB gp3, encrypted | 1.60 | 1.60 |
| Public IPv4 (the Elastic IP), 730 h | 3.65 | 3.65 |
| Backup bucket, about 5 GB S3 Standard, 30-day retention | 0.12 | 0.12 |
| Egress, assumed under the account's free 100 GB a month | 0.00 | 0.00 |
| **Total (estimate)** | **about 19.25** | **about 31.50** |

- **Sizing:** `t4g.small` suits a team of up to about 20 people. Choose
  `t4g.medium` for more, or when the server watcher and dispatcher run.
- **Egress:** the free 100 GB is shared by the whole account. Past it, egress is
  about 0.09 per GB.
- **`cloud secret set`:** each run creates a short-lived Advanced-tier
  parameter for a few seconds, a fraction of a cent.
- **Snapshots** are extra if the user adds an EBS snapshot schedule: about 0.05
  per GB a month of changed data.
- **When billing starts:** at each resource's creation. The Elastic IP is billed
  while allocated, even with the instance stopped.
