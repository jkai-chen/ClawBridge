#!/bin/sh
set -eu
umask 077

PATH=/opt/google-cloud-sdk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
export CLOUDSDK_CORE_DISABLE_PROMPTS=1

fail() {
	printf '%s\n' "clawbridge backup: $*" >&2
	exit 1
}

env_file=${CLAWBRIDGE_ENV_FILE:-/etc/clawbridge/clawbridge.env}
deploy_dir=${CLAWBRIDGE_DEPLOY_DIR:-/opt/clawbridge/repo}
compose_file=$deploy_dir/deploy/compose.yaml
postgres_mount=/var/lib/clawbridge-postgres
postgres_data=$postgres_mount/data

[ -f "$env_file" ] && [ -r "$env_file" ] ||
	fail "private environment file is missing or unreadable"
[ -f "$compose_file" ] ||
	fail "Compose file is missing from the configured deployment directory"

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
if [ "$(count_env_key CLAWBRIDGE_BACKUP_URL)" -gt 0 ]; then
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
if [ "$store" = s3 ]; then
	region=$(read_env_value CLAWBRIDGE_BACKUP_REGION) ||
		fail "private environment must define CLAWBRIDGE_BACKUP_REGION exactly once for an s3:// backup"
	case "$region" in
		[a-z][a-z]-*-[0-9] | [a-z][a-z]-gov-*-[0-9]) ;;
		*) fail "CLAWBRIDGE_BACKUP_REGION is not an AWS region" ;;
	esac
	case "$region" in
		*[!a-z0-9-]*) fail "CLAWBRIDGE_BACKUP_REGION is not an AWS region" ;;
	esac
fi
data_path=$(read_env_value CLAWBRIDGE_POSTGRES_DATA_PATH) ||
	fail "private environment must define CLAWBRIDGE_POSTGRES_DATA_PATH exactly once"
compose_project=$(read_env_value COMPOSE_PROJECT_NAME) ||
	fail "private environment must define COMPOSE_PROJECT_NAME exactly once"

case "$bucket" in
	"" | *[!A-Za-z0-9._-]* | .* | -*)
		fail "CLAWBRIDGE_BACKUP_BUCKET is not a valid bucket name"
		;;
esac
case "$compose_project" in
	"" | [!a-z0-9]* | *[!a-z0-9_-]*)
		fail "COMPOSE_PROJECT_NAME is not a valid Compose project name"
		;;
esac
[ "$data_path" = "$postgres_data" ] ||
	fail "CLAWBRIDGE_POSTGRES_DATA_PATH must be $postgres_data in production"

case "$store" in
	gcs) store_command=gcloud ;;
	s3) store_command=aws ;;
esac
for command_name in docker "$store_command" mountpoint mktemp date awk sha256sum sleep; do
	command -v "$command_name" >/dev/null 2>&1 ||
		fail "required command is unavailable: $command_name"
done

mountpoint -q "$postgres_mount" ||
	fail "Postgres persistent disk is not mounted at $postgres_mount"
[ -d "$postgres_data" ] ||
	fail "Postgres data directory is missing at $postgres_data"

wait_for_db() {
	attempt=1
	while [ "$attempt" -le 60 ]; do
		if docker compose \
			--project-name "$compose_project" \
			--project-directory "$deploy_dir/deploy" \
			--env-file "$env_file" \
			-f "$compose_file" \
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

work_dir=
cleanup() {
	if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
		rm -rf "$work_dir"
	fi
}
trap cleanup 0
trap 'exit 129' 1
trap 'exit 130' 2
trap 'exit 143' 15

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/clawbridge-postgres-backup.XXXXXX") ||
	fail "cannot create secure temporary directory"
timestamp=$(date -u +%Y%m%dT%H%M%SZ) ||
	fail "cannot create UTC backup timestamp"
dump_name=$timestamp.dump
checksum_name=$timestamp.sha256
dump_file=$work_dir/$dump_name
checksum_file=$work_dir/$checksum_name

docker compose \
	--project-name "$compose_project" \
	--project-directory "$deploy_dir/deploy" \
	--env-file "$env_file" \
	-f "$compose_file" \
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

docker compose \
	--project-name "$compose_project" \
	--project-directory "$deploy_dir/deploy" \
	--env-file "$env_file" \
	-f "$compose_file" \
	exec -T db pg_restore --list <"$dump_file" >/dev/null ||
	fail "custom-format archive validation failed"

checksum=$(sha256sum "$dump_file" | awk '{print $1}') ||
	fail "SHA-256 calculation failed"
[ "${#checksum}" -eq 64 ] ||
	fail "SHA-256 calculation produced an invalid digest"
printf '%s  %s\n' "$checksum" "$dump_name" >"$checksum_file"

# The dump first and the checksum last: a backup without its checksum is
# incomplete, and the backup check ignores it.
case "$store" in
	gcs)
		object_base=gs://$bucket/postgres/daily/$timestamp
		upload() { gcloud storage cp --quiet "$1" "$2" >/dev/null; }
		;;
	s3)
		object_base=s3://$bucket/postgres/daily/$timestamp
		upload() { aws s3 cp --only-show-errors --region "$region" "$1" "$2" >/dev/null; }
		;;
esac
upload "$dump_file" "$object_base.dump" ||
	fail "dump upload failed"
upload "$checksum_file" "$object_base.sha256" ||
	fail "checksum upload failed"

printf 'backup=%s.dump\n' "$object_base"
printf 'checksum=%s.sha256\n' "$object_base"
printf 'sha256=%s\n' "$checksum"
