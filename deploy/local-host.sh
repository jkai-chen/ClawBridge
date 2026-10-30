#!/bin/sh
# Runs the ClawBridge server on your own machine (macOS, Linux or WSL2) with
# Docker Compose: deploy/compose.yaml with deploy/compose.local.yaml on top.
# The guide is docs/operations/local-host.md.
#
#   deploy/local-host.sh up --owner EMAIL [--workspace NAME] [--port PORT]
#   deploy/local-host.sh up --lan HOST [--bind IP] [--cert FILE --key FILE] --owner EMAIL [...]
#   deploy/local-host.sh up | status | down | backup | trust | invite-owner [--workspace NAME]
#   deploy/local-host.sh uninstall [--delete-data] [--yes]
#
# Settings: CLAWBRIDGE_LOCAL_DIR (default $HOME/.clawbridge-server) holds the
# env file, backups and the exported root certificate. CLAWBRIDGE_LOCAL_PROJECT
# (default clawbridge-local) names the Compose project on first `up`.
set -eu
umask 077

me=local-host.sh
die() {
	printf '%s: %s\n' "$me" "$*" >&2
	exit 1
}
usage_error() {
	printf '%s: %s\n' "$me" "$*" >&2
	printf 'Run "%s help" for the commands.\n' "$0" >&2
	exit 2
}
say() { printf '%s\n' "$*"; }

usage() {
	cat <<'EOF'
Runs the ClawBridge server on this machine with Docker Compose.

  up --owner EMAIL [--workspace NAME] [--port PORT]
      Shape A: this machine only, on http://127.0.0.1:PORT (default 8443).
  up --lan HOST [--bind IP] [--cert FILE --key FILE] --owner EMAIL [--workspace NAME] [--port PORT]
      Shape B: https://HOST through Caddy on IP:443 (IP defaults to 127.0.0.1;
      pass your LAN address to let colleagues in). With no --cert, Caddy's own
      internal CA signs the certificate: run "trust" next.
  up
      Start again (or upgrade after "git pull") with the saved settings.
  status            The containers and the health check.
  down              Stop the server. The data stays.
  backup            A Postgres dump and its sha256 into $CLAWBRIDGE_LOCAL_DIR/backups.
  trust             Export Caddy's root certificate (shape B without --cert)
                    and print the commands that trust it.
  invite-owner [--workspace NAME]
                    The owner's one-time sign-in code, in your own terminal.
  uninstall [--delete-data] [--yes]
      Stop and remove the containers and the built image. --delete-data also
      deletes the data volumes; it asks you to type the project name, or takes --yes.

Settings: CLAWBRIDGE_LOCAL_DIR (default $HOME/.clawbridge-server),
CLAWBRIDGE_LOCAL_PROJECT (default clawbridge-local, used on the first up).
EOF
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd) || die "cannot find the script's folder"
repo_dir=$(dirname -- "$script_dir")
base_file=$script_dir/compose.yaml
local_file=$script_dir/compose.local.yaml
[ -f "$base_file" ] && [ -f "$local_file" ] ||
	die "deploy/compose.yaml or deploy/compose.local.yaml is missing: run this script from a ClawBridge checkout"

local_dir=${CLAWBRIDGE_LOCAL_DIR:-${HOME:?HOME is not set}/.clawbridge-server}
case "$local_dir" in
	/*) ;;
	*) die "CLAWBRIDGE_LOCAL_DIR must be an absolute path" ;;
esac
env_file=$local_dir/clawbridge.env
backup_dir=$local_dir/backups
root_cert=$local_dir/clawbridge-local-root.crt
# Present once the owner's one-time code was shown; uninstall --delete-data
# removes it, so a fresh database invites the owner again.
invited_marker=$local_dir/owner-invited
tls_dir=$local_dir/tls

# --- checks -----------------------------------------------------------------

need_docker() {
	command -v docker >/dev/null 2>&1 ||
		die "Docker is not installed. Install OrbStack or Docker Desktop (macOS), Docker Engine (Linux), or Docker Desktop's WSL backend or Docker Engine inside WSL (Windows), then run this again."
	compose_version=$(clean_env docker compose version --short 2>/dev/null) ||
		die "Docker Compose v2 (\"docker compose\") is not available. Install the Compose plugin, then run this again."
	compose_new_enough "$compose_version" ||
		die "Docker Compose $compose_version is too old: deploy/compose.local.yaml uses the !override tag, which needs Compose 2.24.4 or newer. Update OrbStack, Docker Desktop or the docker-compose-plugin package, then run this again."
	clean_env docker info >/dev/null 2>&1 ||
		die "Docker is installed but not running, or this user cannot reach it. Start OrbStack, Docker Desktop or the docker service, then run this again."
}

# compose_new_enough VERSION: 2.24.4 or newer. VERSION is what
# "docker compose version --short" prints: 2.29.1, v2.29.1, 2.29.1-desktop.1.
compose_new_enough() {
	v=${1#v}
	v=${v%%[-+]*}
	major=${v%%.*}
	rest=${v#*.}
	minor=${rest%%.*}
	patch=${rest#*.}
	patch=${patch%%.*}
	[ "$rest" != "$v" ] || return 1
	[ "$patch" != "$rest" ] || patch=0
	for n in "$major" "$minor" "$patch"; do
		case "$n" in "" | *[!0123456789]*) return 1 ;; esac
	done
	[ "$major" -gt 2 ] && return 0
	[ "$major" -lt 2 ] && return 1
	[ "$minor" -gt 24 ] && return 0
	[ "$minor" -lt 24 ] && return 1
	[ "$patch" -ge 4 ]
}

valid_email() {
	case "$1" in
		*[!A-Za-z0-9._%+@-]* | *@*@* | @* | *@ | *@.* | *.) return 1 ;;
		*@*.*) return 0 ;;
	esac
	return 1
}

valid_host() { # a DNS name or an IPv4 address
	case "$1" in
		"" | *[!A-Za-z0-9.-]* | .* | -* | *. | *..*) return 1 ;;
	esac
	[ "${#1}" -le 253 ]
}

valid_ipv4() {
	case "$1" in
		"" | *[!0-9.]* | .* | *. | *..*) return 1 ;;
	esac
	old_ifs=$IFS
	IFS=.
	# shellcheck disable=SC2086 # split on dots on purpose
	set -- $1
	IFS=$old_ifs
	[ "$#" -eq 4 ] || return 1
	for octet in "$@"; do
		[ "${#octet}" -le 3 ] && [ "$octet" -le 255 ] || return 1
	done
}

valid_port() {
	case "$1" in
		"" | *[!0-9]* | 0*) return 1 ;;
	esac
	[ "$1" -ge 1024 ] && [ "$1" -le 65535 ]
}

valid_name() { # a workspace or Compose project name
	case "$1" in
		# Letters spelled out: a range like a-z follows the locale's collation.
		"" | [!abcdefghijklmnopqrstuvwxyz0123456789]* | *[!abcdefghijklmnopqrstuvwxyz0123456789_-]*) return 1 ;;
	esac
	[ "${#1}" -le 63 ]
}

# --- the env file -------------------------------------------------------------

read_env() { # KEY: the value, empty when absent
	[ -f "$env_file" ] || return 0
	awk -v wanted="$1" '
		index($0, "=") == 0 || $0 ~ /^[[:space:]]*#/ { next }
		{
			name = substr($0, 1, index($0, "=") - 1)
			if (name == wanted) { value = substr($0, index($0, "=") + 1); found = 1 }
		}
		END { if (found) print value }
	' "$env_file"
}

# open_to_others DIR: group or others may enter it or write in it.
open_to_others() {
	[ -n "$(find "$1" -prune \( -perm -010 -o -perm -001 -o -perm -020 -o -perm -002 \) -print)" ]
}

# The private folder: made 0700 when this script creates it. One that already
# exists keeps its mode, but must be closed to other users: it holds the env
# file and a brought TLS key.
private_dir() {
	if [ ! -d "$1" ]; then
		mkdir -p "$1" || die "cannot create $1"
		chmod 700 "$1" || die "cannot restrict $1"
	fi
	if open_to_others "$1"; then
		die "$1 is open to other users (group or others can enter or write it), and it holds this server's keys. Close it with: chmod 700 $1"
	fi
}

random_hex() { openssl rand -hex 32; }
random_base64() { openssl rand -base64 32; }

write_env_file() { # SHAPE OWNER PORT PROJECT DOMAIN ORIGIN BIND CERT KEY
	command -v openssl >/dev/null 2>&1 ||
		die "openssl is needed to make the server's random keys; install it, then run this again"
	password=$(random_hex) && [ "${#password}" -eq 64 ] || die "openssl rand failed"
	token_key=$(random_base64) && [ "${#token_key}" -eq 44 ] || die "openssl rand failed"
	private_dir "$local_dir"
	tmp=$env_file.partial.$$
	{
		say "# ClawBridge on this machine, written by deploy/local-host.sh. Mode 0600."
		say "# Keep a copy somewhere safe, apart from this machine: without"
		say "# CLAWBRIDGE_LINEAR_APP_TOKEN_KEY (and CLAWBRIDGE_MODEL_KEY_SEAL_KEY, if you"
		say "# add it), a restored backup can't read the Linear app's tokens or model keys."
		say "COMPOSE_PROJECT_NAME=$4"
		say "CLAWBRIDGE_LOCAL_SHAPE=$1"
		say "CLAWBRIDGE_LOCAL_PORT=$3"
		say "CLAWBRIDGE_BOOTSTRAP_OWNER=$2"
		say "CLAWBRIDGE_DOMAIN=$5"
		[ -z "$6" ] || say "CLAWBRIDGE_PUBLIC_ORIGIN=$6"
		[ -z "$7" ] || say "CLAWBRIDGE_BIND_ADDR=$7"
		[ -z "$8" ] || say "CLAWBRIDGE_TLS_CERT=$8"
		[ -z "$9" ] || say "CLAWBRIDGE_TLS_KEY=$9"
		say "POSTGRES_PASSWORD=$password"
		say "# Passed to the server only once CLAWBRIDGE_LINEAR_APP_CLIENT_ID is set"
		say "# (docs/operations/deployment.md, the Linear app)."
		say "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=$token_key"
	} >"$tmp" || die "cannot write $tmp"
	chmod 600 "$tmp" || die "cannot restrict $tmp"
	# Never over an existing file: ln fails if the name is taken.
	if ! ln "$tmp" "$env_file" 2>/dev/null; then
		if [ -e "$env_file" ]; then
			rm -f "$tmp"
			die "$env_file appeared while this ran; run the command again"
		fi
		# This file system has no hard links: create the file with noclobber,
		# which also refuses to write over one.
		say "Note: $local_dir doesn't support hard links; creating the env file with noclobber instead."
		if ! (set -C && cat "$tmp" >"$env_file"); then
			rm -f "$tmp"
			die "cannot create $env_file (it exists, or the folder isn't writable); nothing was overwritten"
		fi
	fi
	rm -f "$tmp"
	unset password token_key
}

# --- Compose ----------------------------------------------------------------

project=
shape=
profiles=

load_settings() {
	[ -f "$env_file" ] ||
		die "no server is set up here ($env_file is missing). Start one with: $0 up --owner EMAIL"
	private_dir "$local_dir"
	project=$(read_env COMPOSE_PROJECT_NAME)
	shape=$(read_env CLAWBRIDGE_LOCAL_SHAPE)
	valid_name "$project" || die "$env_file has no valid COMPOSE_PROJECT_NAME"
	case "$shape" in
		solo) profiles= ;;
		lan)
			if [ -n "$(read_env CLAWBRIDGE_TLS_CERT)" ]; then profiles=lan-cert; else profiles=lan; fi
			;;
		*) die "$env_file has no valid CLAWBRIDGE_LOCAL_SHAPE (solo or lan)" ;;
	esac
}

# Every Docker call runs with only Docker's own settings from this shell: a
# POSTGRES_PASSWORD or CLAWBRIDGE_* in your environment would otherwise beat
# the env file.
clean_env() {
	env -i PATH="$PATH" \
		${HOME+"HOME=$HOME"} \
		${TMPDIR+"TMPDIR=$TMPDIR"} \
		${SSH_AUTH_SOCK+"SSH_AUTH_SOCK=$SSH_AUTH_SOCK"} \
		${XDG_RUNTIME_DIR+"XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR"} \
		${DOCKER_HOST+"DOCKER_HOST=$DOCKER_HOST"} \
		${DOCKER_CONTEXT+"DOCKER_CONTEXT=$DOCKER_CONTEXT"} \
		${DOCKER_CONFIG+"DOCKER_CONFIG=$DOCKER_CONFIG"} \
		${DOCKER_CERT_PATH+"DOCKER_CERT_PATH=$DOCKER_CERT_PATH"} \
		${DOCKER_TLS_VERIFY+"DOCKER_TLS_VERIFY=$DOCKER_TLS_VERIFY"} \
		${DOCKER_API_VERSION+"DOCKER_API_VERSION=$DOCKER_API_VERSION"} \
		${DOCKER_DEFAULT_PLATFORM+"DOCKER_DEFAULT_PLATFORM=$DOCKER_DEFAULT_PLATFORM"} \
		${DOCKER_BUILDKIT+"DOCKER_BUILDKIT=$DOCKER_BUILDKIT"} \
		${BUILDX_BUILDER+"BUILDX_BUILDER=$BUILDX_BUILDER"} \
		"$@"
}

compose() { # [--all-profiles] ARGS...
	profile_args=
	if [ "${1:-}" = --all-profiles ]; then
		shift
		profile_args="--profile lan --profile lan-cert"
	elif [ -n "$profiles" ]; then
		profile_args="--profile $profiles"
	fi
	# shellcheck disable=SC2086 # profile_args is a list of fixed words
	# caddy-cert runs as you, so it can read your 0600 key.
	clean_env CLAWBRIDGE_LOCAL_UID="$(id -u)" CLAWBRIDGE_LOCAL_GID="$(id -g)" \
		docker compose \
		--project-name "$project" \
		--project-directory "$script_dir" \
		--env-file "$env_file" \
		-f "$base_file" -f "$local_file" \
		$profile_args \
		"$@"
}

healthz_url() { printf 'http://127.0.0.1:%s/healthz' "$(read_env CLAWBRIDGE_LOCAL_PORT)"; }

fetch() { # URL: the body on stdout
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 5 "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 5 -O - "$1"
	else
		return 127
	fi
}

wait_healthy() {
	url=$(healthz_url)
	command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
		die "curl or wget is needed to check $url; install one, then run: $0 up"
	# A deadline, not a count of tries: each try may take up to 5 s.
	deadline=$(($(date +%s) + 180))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if body=$(fetch "$url" 2>/dev/null); then
			case "$body" in
				*'"status":"ok"'*) return 0 ;;
			esac
		fi
		sleep 2
	done
	die "the server did not answer $url within 3 minutes. Look at: $0 status, and the api logs with: docker compose -p $project logs api"
}

server_url() {
	if [ "$shape" = solo ]; then
		printf 'http://127.0.0.1:%s' "$(read_env CLAWBRIDGE_LOCAL_PORT)"
	else
		printf 'https://%s' "$(read_env CLAWBRIDGE_DOMAIN)"
	fi
}

volume_exists() { clean_env docker volume inspect "$1" >/dev/null 2>&1; }

# --- commands -----------------------------------------------------------------

cmd_up() {
	shown=0 owner='' workspace=my-team port='' lan_host='' bind='' bind_given=0 cert='' key=''
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--owner) [ "$#" -ge 2 ] || usage_error "--owner needs an email"; owner=$2; shift 2 ;;
			--workspace) [ "$#" -ge 2 ] || usage_error "--workspace needs a name"; workspace=$2; shift 2 ;;
			--port) [ "$#" -ge 2 ] || usage_error "--port needs a number"; port=$2; shift 2 ;;
			--lan) [ "$#" -ge 2 ] || usage_error "--lan needs a host name or address"; lan_host=$2; shift 2 ;;
			--bind) [ "$#" -ge 2 ] || usage_error "--bind needs an IPv4 address"; bind=$2; bind_given=1; shift 2 ;;
			--cert) [ "$#" -ge 2 ] || usage_error "--cert needs a file"; cert=$2; shift 2 ;;
			--key) [ "$#" -ge 2 ] || usage_error "--key needs a file"; key=$2; shift 2 ;;
			*) usage_error "up: unknown argument: $1" ;;
		esac
	done
	[ -z "$owner" ] || valid_email "$owner" || usage_error "--owner is not an email address: $owner"
	valid_name "$workspace" || usage_error "--workspace must be lower-case letters, digits, - and _ (at most 63)"
	[ -z "$port" ] || valid_port "$port" || usage_error "--port must be a number from 1024 to 65535"
	[ -z "$lan_host" ] || valid_host "$lan_host" || usage_error "--lan must be a host name (clawbridge.lan) or an IPv4 address"
	if [ "$bind_given" = 1 ]; then
		case "$bind" in
			"" | 0.0.0.0 | :: | "[::]" | 0 | 0.0.0)
				usage_error "--bind can't be every interface: name the LAN address to publish on (for example 192.0.2.10)" ;;
		esac
		valid_ipv4 "$bind" || usage_error "--bind must be an IPv4 address, such as your LAN address"
	fi
	[ "$bind_given" = 0 ] && [ -z "$cert$key" ] || [ -n "$lan_host" ] || usage_error "--bind, --cert and --key go with --lan"
	if { [ -n "$cert" ] && [ -z "$key" ]; } || { [ -z "$cert" ] && [ -n "$key" ]; }; then
		usage_error "--cert and --key go together"
	fi
	if [ -n "$cert" ]; then
		[ -f "$cert" ] && [ -r "$cert" ] || usage_error "--cert is not a readable file: $cert"
		[ -f "$key" ] && [ -r "$key" ] || usage_error "--key is not a readable file: $key"
	fi
	[ -f "$env_file" ] || [ -n "$owner" ] ||
		usage_error "the first up needs --owner EMAIL: the person who owns this server"
	need_docker

	first_setup=0
	if [ -f "$env_file" ]; then
		load_settings
		# The saved settings decide; a flag that disagrees is a mistake.
		saved_owner=$(read_env CLAWBRIDGE_BOOTSTRAP_OWNER)
		[ -z "$owner" ] || [ "$owner" = "$saved_owner" ] ||
			die "this server's owner is $saved_owner, set at the first up. To start over: $0 uninstall --delete-data, then up again"
		[ -z "$port" ] || [ "$port" = "$(read_env CLAWBRIDGE_LOCAL_PORT)" ] ||
			die "this server uses port $(read_env CLAWBRIDGE_LOCAL_PORT); change CLAWBRIDGE_LOCAL_PORT in $env_file (and CLAWBRIDGE_PUBLIC_ORIGIN in shape A, then every client joins again), or leave out --port"
		if [ -n "$lan_host" ]; then
			[ "$shape" = lan ] && [ "$lan_host" = "$(read_env CLAWBRIDGE_DOMAIN)" ] ||
				die "this server was set up as shape $shape for $(read_env CLAWBRIDGE_DOMAIN); run plain \"$0 up\", or see the guide's \"Changing shape\""
			[ -z "$bind" ] || [ "$bind" = "$(read_env CLAWBRIDGE_BIND_ADDR)" ] ||
				die "this server listens on $(read_env CLAWBRIDGE_BIND_ADDR); change CLAWBRIDGE_BIND_ADDR in $env_file, or leave out --bind"
			if [ -n "$cert" ]; then
				[ "$profiles" = lan-cert ] ||
					die "this server uses Caddy's internal CA; see the guide's \"Changing shape\" to bring your own certificate"
			fi
		elif [ -n "$cert$bind" ]; then
			die "this server was set up as shape $shape; leave out --bind, --cert and --key"
		fi
	else
		first_setup=1
		project=${CLAWBRIDGE_LOCAL_PROJECT:-clawbridge-local}
		valid_name "$project" || die "CLAWBRIDGE_LOCAL_PROJECT must be lower-case letters, digits, - and _"
		port=${port:-8443}
		if [ -n "$lan_host" ]; then
			shape=lan
			bind=${bind:-127.0.0.1}
			tls_cert='' tls_key=''
			if [ -n "$cert" ]; then tls_cert=$tls_dir/cert.pem tls_key=$tls_dir/key.pem; fi
			write_env_file lan "$owner" "$port" "$project" "$lan_host" "https://$lan_host" "$bind" "$tls_cert" "$tls_key"
		else
			shape=solo
			# CLAWBRIDGE_DOMAIN is unused in shape A, but compose.yaml requires it.
			write_env_file solo "$owner" "$port" "$project" 127.0.0.1 "" "" "" ""
		fi
		say "Wrote $env_file (mode 0600). Keep a copy of it somewhere safe."
		load_settings
	fi

	# A brought certificate is copied in, readable by Caddy (which runs with no
	# capabilities) inside the private folder. Run up --cert again to renew.
	if [ -n "$cert" ]; then
		# tls/ is this script's own folder: closed every time, whatever
		# its mode was. Caddy runs as you, so the key stays 0600.
		mkdir -p "$tls_dir" || die "cannot create $tls_dir"
		chmod 700 "$tls_dir" ||
			die "cannot make $tls_dir private (chmod 700), so the key was not copied in"
		rm -f "$tls_dir/cert.pem.partial" "$tls_dir/key.pem.partial"
		if ! (umask 022 && cp "$cert" "$tls_dir/cert.pem.partial") ||
			! (umask 077 && cp "$key" "$tls_dir/key.pem.partial") ||
			! mv "$tls_dir/cert.pem.partial" "$tls_dir/cert.pem" ||
			! mv "$tls_dir/key.pem.partial" "$tls_dir/key.pem"; then
			rm -f "$tls_dir/cert.pem.partial" "$tls_dir/key.pem.partial"
			die "cannot copy the certificate and key into $tls_dir"
		fi
	fi
	if [ "$profiles" = lan-cert ]; then
		[ -f "$tls_dir/cert.pem" ] && [ -f "$tls_dir/key.pem" ] ||
			die "$tls_dir has no cert.pem and key.pem; run up --lan $(read_env CLAWBRIDGE_DOMAIN) --cert FILE --key FILE"
	fi

	# The first start is the one that creates the database volume.
	fresh=1
	if volume_exists "${project}_postgres-data"; then fresh=0; fi
	if [ "$fresh" = 1 ] && [ "$first_setup" = 0 ]; then
		say "The database volume ${project}_postgres-data is new: this starts an empty server with the saved keys."
	fi

	# After a change of shape, the other shape's Caddy must not hold 443.
	case "$profiles" in
		"") others="caddy caddy-cert" ;;
		lan) others=caddy-cert ;;
		lan-cert) others=caddy ;;
	esac
	# shellcheck disable=SC2086 # fixed service names
	compose --all-profiles rm --force --stop $others >/dev/null 2>&1 ||
		die "cannot remove the other shape's Caddy ($others); look at: $0 status"

	say "Building and starting $project (the first build takes a few minutes)..."
	compose up -d --build --remove-orphans ||
		die "docker compose up failed; the output above says why. Fix it and run: $0 up"
	if [ -n "$cert" ]; then
		# A file bind mount keeps the file it was started with: recreate
		# Caddy so it serves the certificate just copied in.
		compose up -d --no-deps --force-recreate caddy-cert ||
			die "Caddy did not restart with the new certificate; run: $0 up --lan $(read_env CLAWBRIDGE_DOMAIN) --cert FILE --key FILE"
	fi
	wait_healthy
	say "The server is up: $(healthz_url) answers."

	owner=$(read_env CLAWBRIDGE_BOOTSTRAP_OWNER)
	ws_owner='' owner_lower=''
	if [ "$fresh" = 1 ] || [ ! -f "$invited_marker" ]; then
		# bootstrap-invite doesn't refuse an owner it let in before: it makes
		# another recovery code. So ask the database first, and treat a
		# workspace this owner already owns as done.
		ws_owner=$(compose exec -T db psql -U clawbridge -d clawbridge -tAq \
			-c "SELECT owner_principal FROM workspaces WHERE name = '$workspace'" 2>/dev/null) || ws_owner=
		ws_owner=$(printf '%s' "$ws_owner" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')
		owner_lower=$(printf '%s' "$owner" | tr '[:upper:]' '[:lower:]')
	fi
	if { [ "$fresh" = 1 ] || [ ! -f "$invited_marker" ]; } && [ -n "$ws_owner" ]; then
		if [ "$ws_owner" = "$owner_lower" ]; then
			: >"$invited_marker"
			say ""
			say "Workspace $workspace already belongs to $owner: no new sign-in code. For a"
			say "lost device, run in your own terminal: $0 invite-owner --workspace $workspace"
		else
			say ""
			say "Workspace $workspace exists with another owner, so no code was made for $owner."
			say "Choose another name in your own terminal: $0 invite-owner --workspace NAME"
		fi
	elif [ "$fresh" = 1 ] || [ ! -f "$invited_marker" ]; then
		if [ -t 0 ] && [ -t 1 ]; then
			say ""
			say "The owner's one-time sign-in code (shown only here, in your terminal):"
			compose run --rm --no-deps api operator bootstrap-invite --workspace "$workspace" --server "$(server_url)" ||
				die "bootstrap-invite failed; run it again with: $0 invite-owner --workspace $workspace"
			: >"$invited_marker"
			shown=1
		else
			say ""
			say "Not a terminal, so no sign-in code was made: the code is a secret and"
			say "goes only to your own terminal. Run there: $0 invite-owner --workspace $workspace"
		fi
	fi
	say ""
	if [ "$shape" = lan ] && [ "$profiles" = lan ]; then
		say "Before signing in, on each computer that uses this server: $0 trust (here), then trust the exported certificate there."
	fi
	# The code's own output already gave the join command.
	if [ "$shown" = 0 ]; then
		say "Next, on the owner's computer, sign in with:"
		say "    clawbridge join --server $(server_url) --email $owner"
	fi
}

cmd_invite_owner() {
	workspace=my-team
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--workspace) [ "$#" -ge 2 ] || usage_error "--workspace needs a name"; workspace=$2; shift 2 ;;
			*) usage_error "invite-owner: unknown argument: $1" ;;
		esac
	done
	valid_name "$workspace" || usage_error "--workspace must be lower-case letters, digits, - and _ (at most 63)"
	[ -t 0 ] && [ -t 1 ] ||
		die "the sign-in code is a secret: run this in your own terminal, not through a pipe or an agent"
	need_docker
	load_settings
	compose run --rm --no-deps api operator bootstrap-invite --workspace "$workspace" --server "$(server_url)" ||
		die "bootstrap-invite failed; is the server up? ($0 status)"
	: >"$invited_marker"
}

cmd_status() {
	[ "$#" -eq 0 ] || usage_error "status takes no arguments"
	need_docker
	load_settings
	compose --all-profiles ps
	url=$(healthz_url)
	if body=$(fetch "$url" 2>/dev/null); then
		say "$url: $body"
	else
		say "$url: no answer. Start the server with: $0 up"
		exit 1
	fi
}

cmd_down() {
	[ "$#" -eq 0 ] || usage_error "down takes no arguments"
	need_docker
	load_settings
	compose --all-profiles down || die "docker compose down failed"
	say "Stopped $project. The data is kept; start it again with: $0 up"
}

cmd_backup() {
	[ "$#" -eq 0 ] || usage_error "backup takes no arguments"
	need_docker
	load_settings
	clean_env ${CLAWBRIDGE_BACKUP_KEEP_DAYS+"CLAWBRIDGE_BACKUP_KEEP_DAYS=$CLAWBRIDGE_BACKUP_KEEP_DAYS"} \
		CLAWBRIDGE_ENV_FILE="$env_file" \
		CLAWBRIDGE_DEPLOY_DIR="$repo_dir" \
		CLAWBRIDGE_COMPOSE_PROJECT="$project" \
		CLAWBRIDGE_COMPOSE_OVERRIDE="$local_file" \
		CLAWBRIDGE_BACKUP_DIR="$backup_dir" \
		sh "$script_dir/backup-postgres.sh" ||
		die "the backup failed; the line above says why. Is the server up? ($0 status)"
	say "Copy $backup_dir and $env_file off this machine: a backup is only as safe as where it lives."
}

cmd_trust() {
	[ "$#" -eq 0 ] || usage_error "trust takes no arguments"
	need_docker
	load_settings
	case "$profiles" in
		"") die "shape A serves plain http on 127.0.0.1: there is nothing to trust" ;;
		lan-cert) die "this server serves your own certificate: trust its CA (mkcert -CAROOT shows mkcert's; a tailscale cert is trusted already)" ;;
	esac
	compose cp caddy:/data/caddy/pki/authorities/local/root.crt "$root_cert.partial" >/dev/null ||
		die "Caddy has not made its root certificate yet. Start the server ($0 up), then run trust again"
	if ! chmod 644 "$root_cert.partial" || ! mv "$root_cert.partial" "$root_cert"; then
		die "cannot write $root_cert"
	fi
	cn=$(openssl x509 -in "$root_cert" -noout -subject -nameopt RFC2253 2>/dev/null | sed -n 's/^subject= *//; s/^.*CN=\([^,]*\).*$/\1/p')
	sha1=$(openssl x509 -in "$root_cert" -noout -fingerprint -sha1 2>/dev/null | sed -n 's/^.*=//; s/://gp')
	cat <<EOF
Exported Caddy's root certificate to:
    $root_cert
Its name: $cn
Its SHA-1 thumbprint: $sha1

Read this first: this root has no name constraints. Anyone who can read the
caddy-data volume on this host (on most hosts, anyone who can use Docker here)
can use it to make a certificate for any site, and a computer that trusts it
will accept that certificate. Trust it only on the computers that sign in to
this server, keep Docker access on this host to yourself, and prefer a
Tailscale or mkcert certificate (--cert, --key) where one fits.

Copy it to each computer that uses this server, and trust it there:
  macOS:          security add-trusted-cert -d -r trustRoot -k ~/Library/Keychains/login.keychain-db clawbridge-local-root.crt
  Debian/Ubuntu:  sudo cp clawbridge-local-root.crt /usr/local/share/ca-certificates/clawbridge-local-root.crt && sudo update-ca-certificates
  Fedora/RHEL:    sudo cp clawbridge-local-root.crt /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust
  Windows:        certutil -user -addstore Root clawbridge-local-root.crt
                  (or PowerShell: Import-Certificate -FilePath clawbridge-local-root.crt -CertStoreLocation Cert:\CurrentUser\Root)
A client inside WSL uses the Linux store; a client on the Windows side uses the Windows store.
Then: clawbridge join --server https://$(read_env CLAWBRIDGE_DOMAIN) --email EMAIL

To stop trusting it later (each computer keeps trusting it until you do):
  macOS:          sudo security remove-trusted-cert -d clawbridge-local-root.crt && security delete-certificate -Z $sha1 ~/Library/Keychains/login.keychain-db
                  (the first command removes the admin trust setting that "trust" added, and needs the certificate
                  file: keep clawbridge-local-root.crt, or run "$0 trust" again to get it)
  Debian/Ubuntu:  sudo rm /usr/local/share/ca-certificates/clawbridge-local-root.crt && sudo update-ca-certificates --fresh
  Fedora/RHEL:    sudo rm /etc/pki/ca-trust/source/anchors/clawbridge-local-root.crt && sudo update-ca-trust
  Windows:        certutil -user -delstore Root "$cn"
                  (or PowerShell: Remove-Item Cert:\\CurrentUser\\Root\\$sha1)
EOF
}

cmd_uninstall() {
	delete_data=0 yes=0
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--delete-data) delete_data=1; shift ;;
			--yes) yes=1; shift ;;
			*) usage_error "uninstall: unknown argument: $1" ;;
		esac
	done
	[ "$yes" = 0 ] || [ "$delete_data" = 1 ] || usage_error "--yes goes with --delete-data"
	need_docker
	load_settings
	if [ "$delete_data" = 1 ]; then
		volumes=
		for v in postgres-data caddy-data caddy-config; do
			if volume_exists "${project}_$v"; then volumes="$volumes ${project}_$v"; fi
		done
		say "This deletes the server's data for good: every workspace, project and message."
		if [ -n "$volumes" ]; then
			say "Volumes to delete:$volumes"
		else
			say "No data volumes exist for $project."
		fi
		say "Kept: $local_dir (the env file, backups and certificates). Delete it yourself if you want it gone."
		if [ "$yes" = 0 ]; then
			[ -t 0 ] || die "not a terminal: pass --yes to confirm, or run this in your own terminal"
			printf 'Type the project name (%s) to delete its data: ' "$project"
			IFS= read -r answer || answer=
			[ "$answer" = "$project" ] || die "not confirmed; nothing was deleted"
		fi
		compose --all-profiles down --volumes --rmi local --remove-orphans ||
			die "docker compose down failed; run uninstall again"
		rm -f "$invited_marker"
		say "Removed $project: its containers, networks, volumes and built image."
		if [ "$shape" = lan ]; then
			say "Each computer that trusted this server's root certificate (Caddy's, or your mkcert root)"
			say "still trusts it until you remove it there: see \"Untrusting it\" in docs/operations/local-host.md."
		fi
	else
		compose --all-profiles down --rmi local --remove-orphans ||
			die "docker compose down failed; run uninstall again"
		say "Removed $project's containers, networks and built image. The data volumes are kept:"
		say "$0 up starts it again with the same data; uninstall --delete-data deletes it."
	fi
}

[ "$#" -gt 0 ] || {
	usage
	exit 2
}
command_name=$1
shift
case "$command_name" in
	up) cmd_up "$@" ;;
	status) cmd_status "$@" ;;
	down) cmd_down "$@" ;;
	backup) cmd_backup "$@" ;;
	trust) cmd_trust "$@" ;;
	invite-owner) cmd_invite_owner "$@" ;;
	uninstall) cmd_uninstall "$@" ;;
	help | -h | --help) usage ;;
	*) usage_error "unknown command: $command_name" ;;
esac
