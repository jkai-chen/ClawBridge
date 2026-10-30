#!/bin/sh
set -eu
umask 077

# Parameters, all optional, read from the environment:
#   CLAWBRIDGE_ENV_FILE         the private env file (default /etc/clawbridge/clawbridge.env)
#   CLAWBRIDGE_DEPLOY_DIR       the repository checkout (default /opt/clawbridge/repo)
#   CLAWBRIDGE_COMPOSE_PROJECT  the Compose project (default: the env file's COMPOSE_PROJECT_NAME)
#   CLAWBRIDGE_COMPOSE_OVERRIDE a second Compose file layered on deploy/compose.yaml
#                               (deploy/local-host.sh passes deploy/compose.local.yaml)
#   CLAWBRIDGE_BACKUP_DIR       a local folder instead of a bucket: the same dump,
#                               validation and sha256, written there
#   CLAWBRIDGE_BACKUP_KEEP_DAYS with CLAWBRIDGE_BACKUP_DIR: remove this folder's dumps
#                               older than this many days (default 14)
# Without CLAWBRIDGE_BACKUP_DIR the bucket path below is the production one.
caller_path=${PATH:-}
PATH=/opt/google-cloud-sdk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
backup_dir=${CLAWBRIDGE_BACKUP_DIR:-}
if [ -n "$backup_dir" ]; then
	# A local folder is run by the person at their own machine (macOS,
	# Linux or WSL), as themselves: their PATH finds their Docker.
	PATH=${caller_path:+$caller_path:}$PATH
fi
export PATH
export CLOUDSDK_CORE_DISABLE_PROMPTS=1

fail() {
	printf '%s\n' "clawbridge backup: $*" >&2
	exit 1
}

# --list prints the store's backup object names (postgres/daily/), nothing
# else, and touches neither the database nor its disk: cloud check backups
# runs it over SSH on a host with no cloud API to ask.
mode=backup
case "${1:-}" in
	"") ;;
	--list) mode=list ;;
	*) fail "usage: backup-postgres.sh [--list]" ;;
esac
# --list names a bucket's objects; a local folder is listed with ls.
[ "$mode" = backup ] || [ -z "$backup_dir" ] ||
	fail "--list reads a bucket: CLAWBRIDGE_BACKUP_DIR is a local folder, so list it with ls"

env_file=${CLAWBRIDGE_ENV_FILE:-/etc/clawbridge/clawbridge.env}
deploy_dir=${CLAWBRIDGE_DEPLOY_DIR:-/opt/clawbridge/repo}
compose_file=$deploy_dir/deploy/compose.yaml
compose_override=${CLAWBRIDGE_COMPOSE_OVERRIDE:-}
postgres_mount=/var/lib/clawbridge-postgres
postgres_data=$postgres_mount/data

[ -f "$env_file" ] && [ -r "$env_file" ] ||
	fail "private environment file is missing or unreadable"
[ -f "$compose_file" ] ||
	fail "Compose file is missing from the configured deployment directory"
if [ -n "$compose_override" ]; then
	case "$compose_override" in
		/*) ;;
		*) compose_override=$deploy_dir/deploy/$compose_override ;;
	esac
	[ -f "$compose_override" ] ||
		fail "the Compose override file is missing: $compose_override"
fi

read_env_value() {
	key=$1
	awk -v wanted="$key" '
		{
			line = $0
			sub(/^[[:space:]]+/, "", line)
			if (line == "" || line ~ /^#/ || index(line, "=") == 0) {
				next
			}
			name = substr(line, 1, index(line, "=") - 1)
			sub(/[[:space:]]+$/, "", name)
			if (name != wanted) {
				next
			}
			found++
			value = substr(line, index(line, "=") + 1)
			sub(/^[[:space:]]+/, "", value)
			sub(/[[:space:]]+$/, "", value)
			if ((value ~ /^".*"$/) || (value ~ /^\047.*\047$/)) {
				value = substr(value, 2, length(value) - 2)
			}
		}
		END {
			if (found != 1) {
				exit 1
			}
			print value
		}
	' "$env_file"
}

count_env_key() {
	awk -v wanted="$1" '
		{
			line = $0
			sub(/^[[:space:]]+/, "", line)
			if (line == "" || line ~ /^#/ || index(line, "=") == 0) {
				next
			}
			name = substr(line, 1, index(line, "=") - 1)
			sub(/[[:space:]]+$/, "", name)
			if (name == wanted) {
				found++
			}
		}
		END { print found + 0 }
	' "$env_file"
}

# The backup store: CLAWBRIDGE_BACKUP_URL (gs://BUCKET or s3://BUCKET), or,
# as before, CLAWBRIDGE_BACKUP_BUCKET for Google Cloud Storage. Never both.
# CLAWBRIDGE_BACKUP_DIR (a local folder) takes neither.
if [ -n "$backup_dir" ]; then
	[ "$(count_env_key CLAWBRIDGE_BACKUP_URL)" -eq 0 ] && [ "$(count_env_key CLAWBRIDGE_BACKUP_BUCKET)" -eq 0 ] ||
		fail "CLAWBRIDGE_BACKUP_DIR is a local folder: the private environment must not also set CLAWBRIDGE_BACKUP_URL or CLAWBRIDGE_BACKUP_BUCKET"
	case "$backup_dir" in
		/*) ;;
		*) fail "CLAWBRIDGE_BACKUP_DIR must be an absolute path" ;;
	esac
	keep_days=${CLAWBRIDGE_BACKUP_KEEP_DAYS:-14}
	case "$keep_days" in
		"" | *[!0-9]* | 0*) fail "CLAWBRIDGE_BACKUP_KEEP_DAYS must be a whole number of days, 1 or more" ;;
	esac
	store=dir
elif [ "$(count_env_key CLAWBRIDGE_BACKUP_URL)" -gt 0 ]; then
	[ "$(count_env_key CLAWBRIDGE_BACKUP_BUCKET)" -eq 0 ] ||
		fail "private environment must set CLAWBRIDGE_BACKUP_URL or CLAWBRIDGE_BACKUP_BUCKET, not both"
	backup_url=$(read_env_value CLAWBRIDGE_BACKUP_URL) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_URL exactly once"
	case "$backup_url" in
		gs://*) store=gcs bucket=${backup_url#gs://} ;;
		s3://*) store=s3 bucket=${backup_url#s3://} ;;
		*) fail "CLAWBRIDGE_BACKUP_URL must be gs://BUCKET or s3://BUCKET" ;;
	esac
else
	bucket=$(read_env_value CLAWBRIDGE_BACKUP_BUCKET) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_URL or CLAWBRIDGE_BACKUP_BUCKET exactly once"
	store=gcs
fi
# An S3-compatible store off this host (ANY-VM-PLAN §4):
# CLAWBRIDGE_BACKUP_ENDPOINT is https://HOST or https://HOST:PORT and
# nothing else, and the store's own key comes from the env file (set with
# clawbridge cloud secret set). Its uploads run in the aws-cli image pinned
# by digest, so the host needs no aws CLI. Without it, s3:// is AWS S3 with
# the instance's role, as before.
endpoint=
if [ "$(count_env_key CLAWBRIDGE_BACKUP_ENDPOINT)" -gt 0 ]; then
	[ "$store" = s3 ] ||
		fail "CLAWBRIDGE_BACKUP_ENDPOINT goes with an s3:// CLAWBRIDGE_BACKUP_URL, not a gs:// one or CLAWBRIDGE_BACKUP_DIR"
	endpoint=$(read_env_value CLAWBRIDGE_BACKUP_ENDPOINT) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_ENDPOINT exactly once"
	case "$endpoint" in
		https://*) endpoint_host=${endpoint#https://} ;;
		*) fail "CLAWBRIDGE_BACKUP_ENDPOINT must be an https:// URL" ;;
	esac
	endpoint_host=${endpoint_host%/}
	case "$endpoint_host" in
		"" | *[!A-Za-z0-9.:-]* | .* | -* | *:*:* | *: | :*)
			fail "CLAWBRIDGE_BACKUP_ENDPOINT must be https://HOST or https://HOST:PORT, with no user, path, query or fragment"
			;;
	esac
	endpoint_name=${endpoint_host%:*}
	case "$endpoint_name" in
		localhost | *.localhost | 127.* | 0.*)
			fail "CLAWBRIDGE_BACKUP_ENDPOINT is this host: a backup must leave it"
			;;
	esac
	access_key=$(read_env_value CLAWBRIDGE_BACKUP_ACCESS_KEY_ID) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_ACCESS_KEY_ID exactly once with CLAWBRIDGE_BACKUP_ENDPOINT"
	secret_key=$(read_env_value CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY exactly once with CLAWBRIDGE_BACKUP_ENDPOINT"
	for key_value in "$access_key" "$secret_key"; do
		case "$key_value" in
			"" | *[!A-Za-z0-9/+=._-]*)
				fail "the backup store's key holds characters a key never has"
				;;
		esac
	done
fi
if [ "$store" = s3 ]; then
	region=$(read_env_value CLAWBRIDGE_BACKUP_REGION) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_REGION exactly once for an s3:// backup"
	if [ -n "$endpoint" ]; then
		# The store's own region name: auto, us-west-004, fr-par.
		case "$region" in
			"" | *[!a-z0-9-]* | -* | *-) fail "CLAWBRIDGE_BACKUP_REGION is not a region name" ;;
		esac
	else
		case "$region" in
			[a-z][a-z]-*-[0-9]) ;; # us-east-1, eu-west-3, us-gov-west-1
			*) fail "CLAWBRIDGE_BACKUP_REGION is not an AWS region" ;;
		esac
		case "$region" in
			*[!a-z0-9-]* | *--*) fail "CLAWBRIDGE_BACKUP_REGION is not an AWS region" ;;
		esac
	fi
fi
if [ "$store" != dir ]; then
	data_path=$(read_env_value CLAWBRIDGE_POSTGRES_DATA_PATH) ||
		fail "private environment must define CLAWBRIDGE_POSTGRES_DATA_PATH exactly once"
fi
if [ -n "${CLAWBRIDGE_COMPOSE_PROJECT:-}" ]; then
	compose_project=$CLAWBRIDGE_COMPOSE_PROJECT
else
	compose_project=$(read_env_value COMPOSE_PROJECT_NAME) ||
		fail "private environment must define COMPOSE_PROJECT_NAME exactly once"
fi

if [ "$store" != dir ]; then
	case "$bucket" in
		"" | *[!A-Za-z0-9._-]* | .* | -*)
			fail "the backup bucket (from CLAWBRIDGE_BACKUP_URL or CLAWBRIDGE_BACKUP_BUCKET) is not a valid bucket name"
			;;
	esac
fi
case "$compose_project" in
	"" | [!a-z0-9]* | *[!a-z0-9_-]*)
		fail "COMPOSE_PROJECT_NAME is not a valid Compose project name"
		;;
esac
if [ "$store" = dir ]; then
	# A local host keeps Postgres on its Compose volume: no data disk to guard.
	for command_name in docker mktemp date awk find sleep; do
		command -v "$command_name" >/dev/null 2>&1 ||
			fail "required command is unavailable: $command_name"
	done
	if command -v sha256sum >/dev/null 2>&1; then
		sha256_of() { sha256sum "$1"; }
	elif command -v shasum >/dev/null 2>&1; then
		sha256_of() { shasum -a 256 "$1"; }
	else
		fail "required command is unavailable: sha256sum (or shasum)"
	fi
	mkdir -p "$backup_dir" ||
		fail "cannot create the backup folder $backup_dir"
else
	[ "$data_path" = "$postgres_data" ] ||
		fail "CLAWBRIDGE_POSTGRES_DATA_PATH must be $postgres_data in production"

	case "$store" in
		gcs) store_command=gcloud ;;
		s3) store_command=aws ;;
	esac
	# An endpoint's uploads run in the aws-cli image: no aws on the host.
	[ -z "$endpoint" ] || store_command=docker
	commands="docker $store_command mktemp awk"
	[ "$mode" = list ] || commands="$commands mountpoint date sha256sum sleep"
	for command_name in $commands; do
		command -v "$command_name" >/dev/null 2>&1 ||
			fail "required command is unavailable: $command_name"
	done
	sha256_of() { sha256sum "$1"; }
fi

# aws_image is amazon/aws-cli 2.37.11, pinned by its multi-arch index
# digest; refresh it with the other image pins before each cut.
aws_image=amazon/aws-cli@sha256:d55d5b391f1cc0d428a05eb07408bb1a730d7df9f656008f1117ed5e13b0c083
work_dir=
cred_dir=
cleanup() {
	if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
		rm -rf "$work_dir"
	fi
	if [ -n "$cred_dir" ] && [ -d "$cred_dir" ]; then
		rm -rf "$cred_dir"
	fi
}
trap cleanup 0
trap 'exit 129' 1
trap 'exit 130' 2
trap 'exit 143' 15

if [ -n "$endpoint" ]; then
	# The endpoint must not be this host, whatever its name says: a backup
	# that stays here dies with it.
	endpoint_addrs=$(getent ahosts "$endpoint_name" | awk '{print $1}' | sort -u) && [ -n "$endpoint_addrs" ] ||
		fail "the backup endpoint's host does not resolve"
	own_addrs=
	if command -v ip >/dev/null 2>&1; then
		own_addrs=$(ip -o addr show | awk '{print $4}' | cut -d/ -f1)
	fi
	for address in $endpoint_addrs; do
		case "$address" in
			127.* | ::1 | 0.* | :: | ::ffff:127.* | ::ffff:0.*) fail "the backup endpoint resolves to loopback: a backup must leave this host" ;;
			fe80:* | FE80:*) fail "the backup endpoint resolves to a link-local address: a backup must leave this host" ;;
		esac
		for own in $own_addrs; do
			[ "$address" != "$own" ] ||
				fail "the backup endpoint resolves to this host: a backup must leave it"
		done
	done
	# The key goes to the container in an env file on tmpfs, root's alone,
	# removed on exit: never in argv, and never on disk.
	cred_dir=$(mktemp -d /run/clawbridge-backup.XXXXXX) ||
		fail "cannot create a private directory under /run for the backup store's key"
	printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$access_key" "$secret_key" >"$cred_dir/aws.env"
	unset access_key secret_key key_value
fi

# aws_run runs the aws CLI in its pinned image against the endpoint, with
# the run's work directory read-only at /backup.
aws_run() {
	docker run --rm --env-file "$cred_dir/aws.env" ${work_dir:+-v "$work_dir:/backup:ro"} \
		"$aws_image" --endpoint-url "$endpoint" --region "$region" "$@"
}

if [ "$mode" = list ]; then
	# Names only, and only names a backup has. A store that can't be read
	# fails, so an empty answer means no backup, never no access.
	case "$store" in
		gcs) names=$(gcloud storage ls "gs://$bucket/postgres/daily/") || fail "cannot list the backup store" ;;
		s3)
			if [ -n "$endpoint" ]; then
				keys=$(aws_run s3api list-objects-v2 --bucket "$bucket" --prefix postgres/daily/ --query "Contents[].Key" --output text) ||
					fail "cannot list the backup store"
			else
				keys=$(aws s3api list-objects-v2 --region "$region" --bucket "$bucket" --prefix postgres/daily/ --query "Contents[].Key" --output text) ||
					fail "cannot list the backup store"
			fi
			names=$(printf '%s\n' "$keys" | tr '\t' '\n' | awk -v base="s3://$bucket/" 'NF {print base $1}')
			;;
	esac
	printf '%s\n' "$names" | grep -E '/postgres/daily/[0-9]{8}T[0-9]{6}Z\.(dump|sha256)$' || true
	exit 0
fi

if [ "$store" != dir ]; then
	mountpoint -q "$postgres_mount" ||
		fail "Postgres persistent disk is not mounted at $postgres_mount"
	[ -d "$postgres_data" ] ||
		fail "Postgres data directory is missing at $postgres_data"
fi

# compose runs the deployment's Compose project on compose.yaml, then
# compose.image.yaml when the server pulls the published image
# (CLAWBRIDGE_IMAGE), as cloud bootstrap does, then the override when one
# is given. Each file is put in front of the Compose arguments, last first.
compose() {
	[ -z "$compose_override" ] || set -- -f "$compose_override" "$@"
	if [ "$(count_env_key CLAWBRIDGE_IMAGE)" -gt 0 ]; then
		set -- -f "$deploy_dir/deploy/compose.image.yaml" "$@"
	fi
	docker compose \
		--project-name "$compose_project" \
		--project-directory "$deploy_dir/deploy" \
		--env-file "$env_file" \
		-f "$compose_file" \
		"$@"
}

wait_for_db() {
	attempt=1
	while [ "$attempt" -le 60 ]; do
		if compose \
			exec -T db pg_isready \
			--username=clawbridge \
			--dbname=clawbridge >/dev/null 2>&1; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 2
	done
	return 1
}

wait_for_db ||
	fail "Postgres did not become ready for backup within 120 seconds"

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/clawbridge-postgres-backup.XXXXXX") ||
	fail "cannot create secure temporary directory"
timestamp=$(date -u +%Y%m%dT%H%M%SZ) ||
	fail "cannot create UTC backup timestamp"
dump_name=$timestamp.dump
checksum_name=$timestamp.sha256
dump_file=$work_dir/$dump_name
checksum_file=$work_dir/$checksum_name

compose \
	exec -T db pg_dump \
	--username=clawbridge \
	--dbname=clawbridge \
	--format=custom \
	--compress=6 \
	--no-owner \
	--no-acl >"$dump_file" ||
	fail "pg_dump failed"
[ -s "$dump_file" ] ||
	fail "pg_dump produced an empty archive"

compose \
	exec -T db pg_restore --list <"$dump_file" >/dev/null ||
	fail "custom-format archive validation failed"

checksum=$(sha256_of "$dump_file" | awk '{print $1}') ||
	fail "SHA-256 calculation failed"
[ "${#checksum}" -eq 64 ] ||
	fail "SHA-256 calculation produced an invalid digest"
printf '%s  %s\n' "$checksum" "$dump_name" >"$checksum_file"

# The dump first and the checksum last: a backup without its checksum is
# incomplete, and the backup check ignores it.
case "$store" in
	dir)
		object_base=$backup_dir/$timestamp
		# Each file lands under a hidden name and is renamed into place, so
		# a half-written file never looks like a backup.
		upload() {
			cp "$1" "$backup_dir/.$(basename "$2").partial" &&
				mv "$backup_dir/.$(basename "$2").partial" "$2"
		}
		;;
	gcs)
		object_base=gs://$bucket/postgres/daily/$timestamp
		upload() { gcloud storage cp --quiet "$1" "$2" >/dev/null; }
		;;
	s3)
		object_base=s3://$bucket/postgres/daily/$timestamp
		if [ -n "$endpoint" ]; then
			upload() { aws_run s3 cp --only-show-errors "/backup/${1##*/}" "$2" >/dev/null; }
		else
			upload() { aws s3 cp --only-show-errors --region "$region" "$1" "$2" >/dev/null; }
		fi
		;;
esac
upload "$dump_file" "$object_base.dump" ||
	fail "dump upload failed"
upload "$checksum_file" "$object_base.sha256" ||
	fail "checksum upload failed"

if [ "$store" = dir ]; then
	# Retention in this folder only, and only for dump/checksum pairs.
	find "$backup_dir" -maxdepth 1 -type f \
		\( -name '[0-9]*T[0-9]*Z.dump' -o -name '[0-9]*T[0-9]*Z.sha256' \) \
		-mtime +"$keep_days" -exec rm -f -- {} + ||
		fail "the backup was written, but removing dumps older than $keep_days days failed"
fi

printf 'backup=%s.dump\n' "$object_base"
printf 'checksum=%s.sha256\n' "$object_base"
printf 'sha256=%s\n' "$checksum"
