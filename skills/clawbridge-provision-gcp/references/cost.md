# Monthly cost estimate (Google Cloud)

An **estimate** in US dollars for one server in a standard-priced region such as
`us-central1` or `europe-west1`, reviewed each release. Prices differ by region
and change over time; the user's billing console is the authority. Show the
user the rows for their chosen VM size, with the total.

| Item | e2-small (2 vCPU shared, 2 GB) | e2-medium (2 vCPU shared, 4 GB) |
|---|---|---|
| VM, on demand, 730 h | 12.50 | 25.00 |
| Boot disk, 20 GB balanced | 2.00 | 2.00 |
| Data disk, 20 GB balanced | 2.00 | 2.00 |
| Static external IPv4, in use | 3.65 | 3.65 |
| Backup bucket, about 5 GB standard, 30-day retention | 0.15 | 0.15 |
| Egress, assumed under 10 GB a month | 1.20 | 1.20 |
| **Total (estimate)** | **about 21.50** | **about 34.00** |

- `e2-small` suits a team of up to about 20 people; choose `e2-medium` for more,
  or when the server watcher and dispatcher run.
- Disk snapshots, if the user adds a snapshot schedule, are extra: about
  0.05 per GB a month of changed data.
- Billing starts when each resource is created, and the static IP is billed
  while reserved even with the VM stopped.
