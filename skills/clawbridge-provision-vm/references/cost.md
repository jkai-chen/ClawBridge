# Monthly cost estimate (your own server)

There is no VM to create on this route: the host is the user's, and so is its
cost. The one new bill is the **backup store**, off the host. Show the user this
table as an **estimate** in US dollars for about 5 GB of backups (30 daily
dumps of a small team's database) and get their yes for the store they choose.
Prices change over time; the provider's pricing page is the authority.

| Store | About 5 GB stored | Notes |
|---|---|---|
| Backblaze B2 | 0.00 (first 10 GB free), then about 0.006 per GB | old versions count as stored until the lifecycle rule removes them |
| Cloudflare R2 | 0.00 (first 10 GB free), then about 0.015 per GB | no egress fee for a restore |
| AWS S3 Standard | about 0.12 | egress for a restore is about 0.09 per GB |
| Wasabi | about 7.00 | billed for at least 1 TB, whatever is stored |

- **The host itself.** If the user is buying a server for this, a small VPS with
  2 vCPUs and 2–4 GB of memory is enough for a team of up to about 20 people;
  its price is the user's choice and isn't part of this estimate. It needs a
  public IPv4 address and a separate block volume for the database (or the
  user's stated opt-out, step 3).
- **When billing starts:** at the bucket's creation, for what it stores.
