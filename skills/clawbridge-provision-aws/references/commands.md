# Commands, step by step

Replace the capitalised placeholders with the user's inputs: `PROFILE`,
`ACCOUNT_ID`, `REGION`, `AZ` (the zone), `DOMAIN`, `OWNER_EMAIL`, `SIZE`,
`BUCKET`. Step 3 adds the IDs AWS assigns (`VPC_ID`, `SUBNET_ID`, `SG_ID`,
`VOLUME_ID`, `INSTANCE_ID`, `ALLOCATION_ID`): copy each from its command's
output into the commands that follow.

The resource names are fixed: the checks and the guide expect them. Every
resource carries the tags `Name` and `clawbridge=1`. The names are:
- `clawbridge-net`, `clawbridge-subnet`, `clawbridge-igw` and `clawbridge-rt` for the network;
- `clawbridge-https` for the security group;
- `clawbridge-vm` for the role and instance profile;
- `clawbridge-data` for the volume;
- `clawbridge-1` for the instance;
- `clawbridge-ip` for the Elastic IP.

Append each name or ID to `created.txt` right after its create succeeds.

Every command below ends with `--profile PROFILE --region REGION` (rule 1). It's
written out each time, never kept in a variable.

## 1. Preflight

```sh
aws sts get-caller-identity --query Account --output text --profile PROFILE --region REGION
aws ec2 describe-availability-zones --zone-names AZ --query 'AvailabilityZones[].State' --output text --profile PROFILE --region REGION
```

The account must be `ACCOUNT_ID`, and the zone `available`. Then confirm that
none of the names exist yet. Each `describe` must print nothing. `get-role` must
fail with `NoSuchEntity`. `head-bucket` tells three cases apart:
- **404:** the name is free.
- **403:** another account owns it; choose another name (rule 5).
- **Success:** it already exists in this account; stop (rule 5).

```sh
aws ec2 describe-instances --filters Name=tag:Name,Values=clawbridge-1 Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text --profile PROFILE --region REGION
aws ec2 describe-vpcs --filters Name=tag:Name,Values=clawbridge-net --query 'Vpcs[].VpcId' --output text --profile PROFILE --region REGION
aws ec2 describe-addresses --filters Name=tag:Name,Values=clawbridge-ip --query 'Addresses[].AllocationId' --output text --profile PROFILE --region REGION
aws ec2 describe-volumes --filters Name=tag:Name,Values=clawbridge-data --query 'Volumes[].VolumeId' --output text --profile PROFILE --region REGION
aws iam get-role --role-name clawbridge-vm --profile PROFILE --region REGION
aws s3api head-bucket --bucket BUCKET --profile PROFILE --region REGION
```

## 3. Provision

**A dedicated network**, so no default VPC rule reaches the instance. One public
subnet, a route to the internet (the instance needs outbound HTTPS for SSM, the
release and certificates), and nothing else:

```sh
aws ec2 create-vpc --cidr-block 10.20.0.0/24 --tag-specifications 'ResourceType=vpc,Tags=[{Key=Name,Value=clawbridge-net},{Key=clawbridge,Value=1}]' --query Vpc.VpcId --output text --profile PROFILE --region REGION
aws ec2 create-subnet --vpc-id VPC_ID --cidr-block 10.20.0.0/24 --availability-zone AZ --tag-specifications 'ResourceType=subnet,Tags=[{Key=Name,Value=clawbridge-subnet},{Key=clawbridge,Value=1}]' --query Subnet.SubnetId --output text --profile PROFILE --region REGION
aws ec2 create-internet-gateway --tag-specifications 'ResourceType=internet-gateway,Tags=[{Key=Name,Value=clawbridge-igw},{Key=clawbridge,Value=1}]' --query InternetGateway.InternetGatewayId --output text --profile PROFILE --region REGION
aws ec2 attach-internet-gateway --internet-gateway-id IGW_ID --vpc-id VPC_ID --profile PROFILE --region REGION
aws ec2 create-route-table --vpc-id VPC_ID --tag-specifications 'ResourceType=route-table,Tags=[{Key=Name,Value=clawbridge-rt},{Key=clawbridge,Value=1}]' --query RouteTable.RouteTableId --output text --profile PROFILE --region REGION
aws ec2 create-route --route-table-id RT_ID --destination-cidr-block 0.0.0.0/0 --gateway-id IGW_ID --profile PROFILE --region REGION
aws ec2 associate-route-table --route-table-id RT_ID --subnet-id SUBNET_ID --profile PROFILE --region REGION
```

**The security group:** TCP 443 from anywhere, and nothing else in. A new group
has no inbound rules, and outbound is open, as on any VPC:

```sh
aws ec2 create-security-group --group-name clawbridge-https --description 'ClawBridge: HTTPS only' --vpc-id VPC_ID --tag-specifications 'ResourceType=security-group,Tags=[{Key=Name,Value=clawbridge-https},{Key=clawbridge,Value=1}]' --query GroupId --output text --profile PROFILE --region REGION
aws ec2 authorize-security-group-ingress --group-id SG_ID --ip-permissions 'IpProtocol=tcp,FromPort=443,ToPort=443,IpRanges=[{CidrIp=0.0.0.0/0}]' --profile PROFILE --region REGION
aws ec2 describe-security-groups --group-ids SG_ID --query 'SecurityGroups[].IpPermissions' --output json --profile PROFILE --region REGION
```

The last command must show exactly one rule: tcp 443 from `0.0.0.0/0`.

**The backup bucket:** private, versioned, encrypted, keeping 30 days. In
`us-east-1`, leave out `--create-bucket-configuration`. Write `lifecycle.json`
in your working directory, with this content (the release's
`deploy/s3-lifecycle.json`):

```json
{
  "Rules": [
    {
      "ID": "clawbridge-postgres-daily",
      "Filter": {"Prefix": "postgres/daily/"},
      "Status": "Enabled",
      "Expiration": {"Days": 30},
      "NoncurrentVersionExpiration": {"NoncurrentDays": 7},
      "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}
    }
  ]
}
```

```sh
aws s3api create-bucket --bucket BUCKET --create-bucket-configuration LocationConstraint=REGION --profile PROFILE --region REGION
aws s3api put-public-access-block --bucket BUCKET --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true --profile PROFILE --region REGION
aws s3api put-bucket-versioning --bucket BUCKET --versioning-configuration Status=Enabled --profile PROFILE --region REGION
aws s3api put-bucket-encryption --bucket BUCKET --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}' --profile PROFILE --region REGION
aws s3api put-bucket-lifecycle-configuration --bucket BUCKET --lifecycle-configuration file://lifecycle.json --profile PROFILE --region REGION
```

**The instance's role,** reachable by SSM and nothing else yet. Its own policy
follows once the instance ID is known. Write `ec2-trust.json`:

```json
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}]}
```

```sh
aws iam create-role --role-name clawbridge-vm --assume-role-policy-document file://ec2-trust.json --tags Key=clawbridge,Value=1 --profile PROFILE --region REGION
aws iam attach-role-policy --role-name clawbridge-vm --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore --profile PROFILE --region REGION
aws iam create-instance-profile --instance-profile-name clawbridge-vm --profile PROFILE --region REGION
aws iam add-role-to-instance-profile --instance-profile-name clawbridge-vm --role-name clawbridge-vm --profile PROFILE --region REGION
```

**The data volume,** encrypted, in the instance's zone:

```sh
aws ec2 create-volume --availability-zone AZ --size 20 --volume-type gp3 --encrypted --tag-specifications 'ResourceType=volume,Tags=[{Key=Name,Value=clawbridge-data},{Key=clawbridge,Value=1}]' --query VolumeId --output text --profile PROFILE --region REGION
```

**The instance:** Amazon Linux 2023 on arm64. It ships the SSM agent and the AWS
CLI, which `cloud secret set` uses on the instance.
- **Metadata:** IMDSv2 only.
- **Root volume:** encrypted gp3.
- **Access:** no key pair and no public IP of its own; the Elastic IP follows.

If `run-instances` says the instance profile is invalid, IAM has not spread it
yet: wait ten seconds and run it again.

```sh
aws ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text --profile PROFILE --region REGION
aws ec2 run-instances --image-id AMI_ID --instance-type SIZE --subnet-id SUBNET_ID --security-group-ids SG_ID --iam-instance-profile Name=clawbridge-vm --metadata-options HttpTokens=required,HttpEndpoint=enabled --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=20,VolumeType=gp3,Encrypted=true,DeleteOnTermination=true}' --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=clawbridge-1},{Key=clawbridge,Value=1}]' --query 'Instances[0].InstanceId' --output text --profile PROFILE --region REGION
aws ec2 wait instance-running --instance-ids INSTANCE_ID --profile PROFILE --region REGION
aws ec2 attach-volume --volume-id VOLUME_ID --instance-id INSTANCE_ID --device /dev/sdf --profile PROFILE --region REGION
aws ec2 allocate-address --domain vpc --tag-specifications 'ResourceType=elastic-ip,Tags=[{Key=Name,Value=clawbridge-ip},{Key=clawbridge,Value=1}]' --query '[AllocationId,PublicIp]' --output text --profile PROFILE --region REGION
aws ec2 associate-address --allocation-id ALLOCATION_ID --instance-id INSTANCE_ID --profile PROFILE --region REGION
```

**The role's own policy,** now that the instance ID is known. Write
`role-policy.json` from this template (the release's
`deploy/aws-instance-role-policy.json`), replacing `REGION`, `ACCOUNT_ID`,
`INSTANCE_ID` and `BUCKET`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "WriteDailyBackups",
      "Effect": "Allow",
      "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::BUCKET/postgres/daily/*"
    },
    {
      "Sid": "ReadOwnSecretParameters",
      "Effect": "Allow",
      "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:REGION:ACCOUNT_ID:parameter/clawbridge/INSTANCE_ID/*"
    },
    {
      "Sid": "DecryptOnlyThroughSSM",
      "Effect": "Allow",
      "Action": "kms:Decrypt",
      "Resource": "*",
      "Condition": {"StringEquals": {"kms:ViaService": "ssm.REGION.amazonaws.com"}}
    }
  ]
}
```

```sh
aws iam put-role-policy --role-name clawbridge-vm --policy-name clawbridge --policy-document file://role-policy.json --profile PROFILE --region REGION
```

Show the user the Elastic IP (the `PublicIp` printed by `allocate-address`). Ask
them to create an `A` record for `DOMAIN` with it at their DNS provider. If
their DNS is in Route 53, offer to create it with `aws route53
change-resource-record-sets`; that's a change in their account, so it needs its
own yes.

## Running a block on the instance

Steps 4 and 6 run blocks of commands on the instance, as root, through SSM.
1. **Write the file.** Write each block to a file in your working directory,
   such as `step4-disk.sh`.
2. **Send it.** Send it with the user's yes:

   ```sh
   python3 -c 'import json,sys; print(json.dumps({"commands": open(sys.argv[1]).read().splitlines(), "executionTimeout": ["1800"]}))' step4-disk.sh > step4-disk.json
   aws ssm send-command --instance-ids INSTANCE_ID --document-name AWS-RunShellScript --parameters file://step4-disk.json --query Command.CommandId --output text --profile PROFILE --region REGION
   ```

3. **Read the result.** Poll every few seconds until `Status` is `Success` or a
   failure, then report the output verbatim:

   ```sh
   aws ssm get-command-invocation --command-id COMMAND_ID --instance-id INSTANCE_ID --query '[Status,StandardOutputContent,StandardErrorContent]' --output text --profile PROFILE --region REGION
   ```

SSM keeps every block and its output in the account's history. A block never
contains a secret and never prints one (rule 3).

## 4. Prepare the instance

**The data volume.** The block formats the volume only when it has no filesystem,
and adds the `/etc/fstab` line only once, so running it again is safe. Its
device name is the volume ID without the dash, for example `vol0123456789abcdef0`
for `vol-0123456789abcdef0`.
- **After a format:** append `formatted clawbridge-data` to `created.txt`.
- **`REUSE=1`** only when `created.txt` already records `formatted
  clawbridge-data`, that is, when you formatted it on an earlier run. Otherwise
  leave `REUSE=0`.
- **If it prints STOP … already has a filesystem**, it exited without touching
  `/etc/fstab` or mounting anything: the volume holds data you didn't create.
  Stop and ask (rule 5).

```sh
set -e
REUSE=0
DISK=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_VOLUME_ID_WITHOUT_DASH
test -b "$DISK" || { echo "STOP: $DISK is not attached"; exit 1; }
if blkid "$DISK" >/dev/null; then
  [ "$REUSE" = 1 ] || { echo "STOP: $DISK already has a filesystem; not formatting or mounting it"; exit 2; }
  echo "reusing the filesystem formatted on an earlier run"
else
  mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$DISK"
fi
mkdir -p /var/lib/clawbridge-postgres
if ! grep -q ' /var/lib/clawbridge-postgres ' /etc/fstab; then
  u=$(blkid -s UUID -o value "$DISK")
  [ -n "$u" ]
  echo "UUID=$u /var/lib/clawbridge-postgres ext4 discard,defaults,nofail 0 2" >> /etc/fstab
fi
mount -a
findmnt -n -o SOURCE /var/lib/clawbridge-postgres
readlink -f "$DISK"
```

The last two lines must print the same device: the mount is the data volume.
`STOP: … is not attached` means the volume isn't attached yet, or its ID is
mistyped. Nothing was written, so check `attach-volume` and run the block
again.

**Docker.** Docker comes from Amazon Linux's own package. The Compose and Buildx
plugins come from their projects' releases, pinned by version and checked
against the published checksums:

```sh
set -e
dnf install -y docker git
systemctl enable --now docker
mkdir -p /usr/local/lib/docker/cli-plugins
cd /usr/local/lib/docker/cli-plugins
COMPOSE=v2.29.7
curl -fsSLo docker-compose "https://github.com/docker/compose/releases/download/$COMPOSE/docker-compose-linux-aarch64"
echo "$(curl -fsSL "https://github.com/docker/compose/releases/download/$COMPOSE/docker-compose-linux-aarch64.sha256" | cut -d' ' -f1)  docker-compose" | sha256sum -c
BUILDX=v0.17.1
curl -fsSLo docker-buildx "https://github.com/docker/buildx/releases/download/$BUILDX/buildx-$BUILDX.linux-arm64"
echo "$(curl -fsSL "https://github.com/docker/buildx/releases/download/$BUILDX/checksums.txt" | grep " \*buildx-$BUILDX.linux-arm64$" | cut -d' ' -f1)  docker-buildx" | sha256sum -c
chmod 0755 docker-compose docker-buildx
docker compose version
docker buildx version
```

A checksum line that doesn't print `OK` ends the block with an error: stop and
ask (rule 5).

**The release.** Check out the tag of the user's own ClawBridge release, never
a branch. On the user's machine, `clawbridge version` prints its `version` (the
tag) and `commit`:

```sh
TAG=vX.Y.Z
git clone --branch "$TAG" --depth 1 https://github.com/jkai-chen/ClawBridge.git /opt/clawbridge/repo
git -C /opt/clawbridge/repo rev-parse HEAD
```

The printed commit must equal the `commit` from `clawbridge version`. If it
differs, stop and ask (rule 5).

## 6. Install the server

**The env file,** created once, root-only. The database password is generated
here and never printed. Add the sign-in lines from [signin.md](signin.md) for
the chosen path (the device path adds `CLAWBRIDGE_AUTH=device`). On AWS the
backups go to S3, named by `CLAWBRIDGE_BACKUP_URL` and
`CLAWBRIDGE_BACKUP_REGION`:

```sh
install -d -o root -g root -m 0755 /etc/clawbridge
if test -e /etc/clawbridge/clawbridge.env; then
  echo "STOP: /etc/clawbridge/clawbridge.env already exists; not replacing it"
else
  sh -c 'umask 077; {
  printf "POSTGRES_PASSWORD=%s\n" "$(openssl rand -hex 32)"
  printf "CLAWBRIDGE_DOMAIN=%s\n" DOMAIN
  printf "CLAWBRIDGE_BOOTSTRAP_OWNER=%s\n" OWNER_EMAIL
  printf "CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data\n"
  printf "CLAWBRIDGE_BACKUP_URL=s3://%s\n" BUCKET
  printf "CLAWBRIDGE_BACKUP_REGION=%s\n" REGION
  printf "COMPOSE_PROJECT_NAME=clawbridge\n"
} > /etc/clawbridge/clawbridge.env'
fi
```

If it prints STOP, an env file already exists: stop and ask (rule 5). Add
further lines with `tee -a`, never by rewriting the file.

**The disk guard and backups.** Follow "Persistent disk and backup guards" in
`/opt/clawbridge/repo/docs/operations/deployment.md`, as blocks on the instance,
skipping its `/etc/fstab` and symlink parts, which this playbook already did.
It makes Docker start only with the data volume mounted, and installs the daily
backup timer. The backup uploads to S3 with the instance's role.

**Start the stack:**

```sh
docker compose --env-file /etc/clawbridge/clawbridge.env --project-name clawbridge \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d --build
```

Caddy obtains the certificate within a minute or two, once DNS points at the
Elastic IP. Then, from the user's machine:

```sh
clawbridge cloud check server version --server https://DOMAIN \
  --instance INSTANCE_ID --region REGION --profile PROFILE --account ACCOUNT_ID
```

**The first backup,** so the backups check has something to see:

```sh
systemctl start clawbridge-postgres-backup.service
```
