#!/bin/sh
# Downloaded text is data. Never source or evaluate it. The binary runs only
# after its checksum matched and it is installed, for `skill install` (--no-skill
# skips that).
set -eu
umask 077
fail() { printf 'clawbridge install: %s\n' "$*" >&2; exit 1; }
valid_version() {
  case "$1" in ''|*[!v0-9.]*) return 1 ;; esac
  printf '%s\n' "$1" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
}
# version_older A B: A is an older release than B. Both are canonical.
version_older() {
  printf '%s %s\n' "${1#v}" "${2#v}" | LC_ALL=C awk '{
    split($1, a, "."); split($2, b, ".")
    for (i = 1; i <= 3; i++) if (a[i] + 0 != b[i] + 0) exit !(a[i] + 0 < b[i] + 0)
    exit 1 }'
}
install_version=''
install_bin_dir=${HOME:+$HOME/.local/bin}
install_skill=1
install_allow_downgrade=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help|-h)
      printf '%s\n' 'Usage: sh install.sh [--version vX.Y.Z] [--bin-dir ABSOLUTE_PATH] [--no-skill] [--allow-downgrade]' 'Default: $HOME/.local/bin/clawbridge, then `clawbridge skill install` for the agents configured here.' 'An older version than the installed one needs --allow-downgrade.' 'CLAWBRIDGE_VERIFY=required also checks the build provenance with the GitHub CLI (gh attestation verify).' 'No PATH/configuration/sign-in changes.'
      exit 0 ;;
    --allow-downgrade)
      install_allow_downgrade=1
      shift ;;
    --version)
      [ "$#" -ge 2 ] && [ -z "$install_version" ] || fail '--version requires one value, once'
      install_version=$2
      valid_version "$install_version" || fail 'invalid canonical release version'
      shift 2 ;;
    --no-skill)
      install_skill=0
      shift ;;
    --bin-dir)
      [ "$#" -ge 2 ] || fail '--bin-dir requires a value'
      install_bin_dir=$2
      shift 2 ;;
    *) fail 'unknown argument; use --help' ;;
  esac
done
case "$install_bin_dir" in /*) ;; *) fail 'bin directory must be absolute' ;; esac
# A slash suffix must not hide a directory symlink from test -L.
while [ "$install_bin_dir" != / ] && [ "${install_bin_dir%/}" != "$install_bin_dir" ]; do
  install_bin_dir=${install_bin_dir%/}
done
install_os=$(uname -s)
case "$install_os" in Darwin) install_os=darwin ;; Linux) install_os=linux ;; *) fail 'supported systems are Darwin and Linux' ;; esac
install_arch=$(uname -m)
case "$install_arch" in x86_64|amd64) install_arch=amd64 ;; arm64|aarch64) install_arch=arm64 ;; *) fail 'supported architectures are amd64 and arm64' ;; esac
command -v curl >/dev/null 2>&1 || fail 'curl is required'
# Build provenance (G9-10): optional, and required only when asked for, since
# it needs the GitHub CLI. The checksum alone proves the bytes match the
# release's manifest, not who built them.
case "${CLAWBRIDGE_VERIFY:-}" in
  '') install_verify=0 ;;
  required) install_verify=1
    command -v gh >/dev/null 2>&1 || fail 'CLAWBRIDGE_VERIFY=required needs the GitHub CLI (gh) on PATH' ;;
  *) fail 'CLAWBRIDGE_VERIFY must be unset or "required"' ;;
esac
if command -v sha256sum >/dev/null 2>&1; then install_hash=sha256sum
elif command -v shasum >/dev/null 2>&1; then install_hash=shasum
else fail 'sha256sum or shasum is required'; fi
check_destination() {
  [ ! -L "$install_bin_dir" ] || fail 'bin directory must not be a symlink'
  [ ! -e "$install_bin_dir" ] || [ -d "$install_bin_dir" ] || fail 'bin directory is not a directory'
  [ ! -L "$install_bin_dir/clawbridge" ] || fail 'destination must not be a symlink'
  [ ! -e "$install_bin_dir/clawbridge" ] || [ -f "$install_bin_dir/clawbridge" ] || fail 'destination must be a regular file'
}
check_destination
install_temp=''
install_stage=''
cleanup() {
  [ -z "$install_stage" ] || rm -f -- "$install_stage"
  [ -z "$install_temp" ] || rm -rf -- "$install_temp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
install_temp=$(mktemp -d "${TMPDIR:-/tmp}/clawbridge-install.XXXXXX")
fetch() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 10 --max-time 120 --max-redirs 5 "$@"
}
install_base=https://github.com/jkai-chen/ClawBridge/releases
if [ -z "$install_version" ]; then
  install_latest=$(fetch --max-filesize 1048576 --output /dev/null --write-out '%{url_effective}' "$install_base/latest") || fail 'latest release discovery failed'
  case "$install_latest" in "$install_base/tag/"*) install_version=${install_latest#"$install_base/tag/"} ;; *) fail 'unexpected latest release location' ;; esac
  valid_version "$install_version" || fail 'latest release is not a canonical stable version'
fi
# Never silently older than what is installed: a replaced "latest" must not
# roll an install back. A binary that doesn't say its version (a dev build,
# or no binary) doesn't stop the install.
if [ "$install_allow_downgrade" = 0 ] && [ -f "$install_bin_dir/clawbridge" ] && [ -x "$install_bin_dir/clawbridge" ]; then
  install_current=$("$install_bin_dir/clawbridge" version 2>/dev/null | LC_ALL=C sed -n 's/.*"version":"\(v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)".*/\1/p' | head -n 1) || install_current=''
  if valid_version "$install_current" && version_older "$install_version" "$install_current"; then
    fail "$install_version is older than the installed $install_current; pass --allow-downgrade to install it anyway"
  fi
fi
install_asset="clawbridge_${install_version}_${install_os}_${install_arch}"
install_url="$install_base/download/$install_version"
fetch --max-filesize 1048576 --output "$install_temp/SHA256SUMS" "$install_url/SHA256SUMS" || fail 'manifest download failed'
install_digest=$(LC_ALL=C awk -v wanted="$install_asset" '
  { hash=substr($0,1,64); name=substr($0,67)
    if (length(hash)!=64 || hash !~ /^[0-9a-f]+$/ || substr($0,65,2)!="  " || name !~ /^[A-Za-z0-9_.-]+$/ || seen[name]++) bad=1
    if (name==wanted) { count++; selected=hash }
  }
  END { if (bad || count!=1) exit 1; print selected }
' "$install_temp/SHA256SUMS") || fail 'manifest must contain exactly one valid selected digest and no malformed/duplicate entries'
fetch --max-filesize 536870912 --output "$install_temp/binary" "$install_url/$install_asset" || fail 'binary download failed'
[ -s "$install_temp/binary" ] || fail 'downloaded binary is empty'
if [ "$install_hash" = sha256sum ]; then
  install_actual=$(sha256sum "$install_temp/binary")
else install_actual=$(shasum -a 256 "$install_temp/binary"); fi
install_actual=${install_actual%% *}
[ "$install_actual" = "$install_digest" ] || fail 'binary checksum mismatch'
if [ "$install_verify" = 1 ]; then
  gh attestation verify "$install_temp/binary" --repo jkai-chen/ClawBridge \
    --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref "refs/tags/$install_version" >/dev/null 2>&1 ||
    fail "the build provenance of $install_version could not be verified (gh attestation verify); nothing was installed"
  printf 'Verified the build provenance of %s\n' "$install_version"
fi
check_destination
mkdir -p -- "$install_bin_dir"
install_stage=$(mktemp "$install_bin_dir/.clawbridge-install.XXXXXX")
cp "$install_temp/binary" "$install_stage"
chmod 755 "$install_stage"
check_destination
mv -f -- "$install_stage" "$install_bin_dir/clawbridge"
install_stage=''
printf 'Installed %s at %s\n' "$install_version" "$install_bin_dir/clawbridge"
# The agent skill is a convenience: a failure warns and never fails the install.
if [ "$install_skill" = 1 ]; then
  if "$install_bin_dir/clawbridge" skill install >/dev/null; then
    printf '%s\n' 'Installed the ClawBridge agent skill (see: clawbridge skill status).'
  else
    printf '%s\n' 'clawbridge install: warning: the agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.' >&2
  fi
fi
case ":${PATH:-}:" in *":$install_bin_dir:"*) ;; *)
  install_quoted=$(printf '%s' "$install_bin_dir" | sed "s/'/'\\\\''/g")
  printf "For this shell, run: export PATH='%s':\"\$PATH\"\n" "$install_quoted"
  ;;
esac
