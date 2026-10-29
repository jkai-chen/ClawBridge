#!/usr/bin/env bash
# Checks the server image's Dockerfile and build, with no registry writes.
#   bases [--strict]  each FROM is pinned by a multi-arch index digest; a pin
#                     behind its tag is reported (an error with --strict, the
#                     release checklist's refresh step)
#   arch              builds linux/amd64 and linux/arm64 and checks that each
#                     binary and each base's packages are that architecture
set -euo pipefail
cd "$(dirname "$0")/.."
die() { echo "check-image: $*" >&2; exit 1; }

bases() {
  local strict=${1:-} stale=0 ref name digest kind current
  while read -r ref; do
    [[ $ref == *@sha256:* ]] || die "base not pinned by digest: $ref"
    name=${ref%@*} digest=${ref#*@}
    kind=$(docker buildx imagetools inspect --raw "$ref" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("mediaType",""))')
    case $kind in
      application/vnd.oci.image.index.v1+json|application/vnd.docker.distribution.manifest.list.v2+json) ;;
      *) die "base pinned by a platform manifest, not an index: $ref ($kind)" ;;
    esac
    current=$(docker buildx imagetools inspect --format '{{json .Manifest}}' "$name" | python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])')
    if [[ $current != "$digest" ]]; then
      echo "check-image: $name has moved to $current" >&2
      stale=1
    fi
  done < <(sed -n 's/^FROM \(--platform=[^ ]* \)\{0,1\}\([^ ]*\).*/\2/p' Dockerfile)
  [[ $stale == 0 || $strict != --strict ]] || die 'refresh the base digests before the cut'
  echo 'check-image: bases pinned by index digest'
}

arch() {
  local work platform want elf
  work=$(mktemp -d)
  trap 'rm -rf -- "$work"' RETURN
  for platform in amd64 arm64; do
    mkdir "$work/$platform"
    docker buildx build --quiet --platform "linux/$platform" --output "type=tar,dest=$work/$platform.tar" . >/dev/null
    tar -xf "$work/$platform.tar" -C "$work/$platform" clawbridge var/lib/dpkg/status.d
    case $platform in amd64) want=x86-64 ;; arm64) want=aarch64 ;; esac
    elf=$(file -b "$work/$platform/clawbridge")
    [[ $elf == *"$want"* && $elf == *"statically linked"* ]] || die "linux/$platform binary is: $elf"
    # Every package of the base names this architecture, or "all".
    if grep -h '^Architecture:' "$work/$platform"/var/lib/dpkg/status.d/* | grep -vxE "Architecture: ($platform|all)"; then
      die "linux/$platform has another architecture's base"
    fi
    echo "check-image: linux/$platform ok ($want, static)"
  done
}

case ${1:-} in
  bases) bases "${2:-}" ;;
  arch) arch ;;
  *) die 'usage: check-image.sh bases [--strict] | arch' ;;
esac
