#!/usr/bin/env bash
# Build only an exact, clean, canonical release tag. Never runs target binaries.
set -euo pipefail
export LC_ALL=C
# Empty Go variables otherwise fall back to caller defaults in the GOENV file.
export GOENV=off GOFLAGS='' GOWORK=off
die() { printf 'build-release: %s\n' "$*" >&2; exit 1; }
[[ $# == 2 ]] || die 'usage: build-release.sh VERSION OUTPUT_DIRECTORY'
release_version=$1
[[ $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || die 'invalid canonical release version'
release_root=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$release_root"
release_commit=$(git rev-parse --verify HEAD)
[[ $(git rev-parse --verify "refs/tags/$release_version^{commit}") == "$release_commit" ]] || die 'tag must resolve to HEAD'
[[ -z $(git status --porcelain --untracked-files=all) ]] || die 'source must be clean, including nonignored untracked files'
release_time=$(git show -s --format=%ct "$release_commit")
# Go accepts this UTC timestamp on every build platform. Python is also used by
# the existing integration runner and the release verification scripts.
release_time=$(python3 -c 'import datetime,sys; print(datetime.datetime.fromtimestamp(int(sys.argv[1]),datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$release_time")
release_output=$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$2")
[[ $release_output != "$release_root" ]] || die 'output cannot be source root'
case "$release_output/" in
  "$release_root/"*)
    release_relative=${release_output#"$release_root/"}
    git check-ignore -q -- "$release_relative/" || die 'output in source checkout must be explicitly ignored'
    ;;
esac
[[ ! -e $release_output || ( -d $release_output && -z $(ls -A "$release_output") ) ]] || die 'output must be absent or empty'
mkdir -p "$release_output"
release_flags="-X clawbridge/internal/buildinfo.Version=$release_version -X clawbridge/internal/buildinfo.Commit=$release_commit -X clawbridge/internal/buildinfo.BuildTime=$release_time"
for release_target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  release_os=${release_target%/*}
  release_arch=${release_target#*/}
  release_name="clawbridge_${release_version}_${release_os}_${release_arch}"
  [[ $release_os != windows ]] || release_name+=.exe
  CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" GOAMD64=v1 GOARM64=v8.0 \
    go build -trimpath -ldflags "$release_flags" -o "$release_output/$release_name" ./cmd/clawbridge
done
cp LICENSE scripts/install.sh scripts/install.ps1 "$release_output/"
printf '%s\n' "$release_commit" > "$release_output/SOURCE-COMMIT.txt"
python3 - "$release_output" <<'PY'
import hashlib,pathlib,sys
p=pathlib.Path(sys.argv[1])
files=sorted(p.iterdir(),key=lambda f:f.name)
assert len(files)==10 and all(f.is_file() for f in files)
(p/'SHA256SUMS').write_text(''.join(hashlib.sha256(f.read_bytes()).hexdigest()+'  '+f.name+'\n' for f in files),encoding='ascii')
PY
printf 'Built %s at %s into %s\n' "$release_version" "$release_commit" "$release_output"
