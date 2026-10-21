#!/usr/bin/env bash
# CI-only publication, after independent tests. Never replaces a release/asset.
set -euo pipefail
[[ $# == 2 ]] || { echo 'usage: publish-release.sh VERSION ASSET_DIRECTORY' >&2; exit 1; }
release_version=$1
[[ $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || exit 1
[[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF:-} == "refs/tags/$release_version" && ${GITHUB_REPOSITORY:-} == jkai-chen/ClawBridge ]] || { echo 'publication requires the exact repository tag-push CI context' >&2; exit 1; }
export GH_REPO=jkai-chen/ClawBridge
release_assets=$(cd "$2" && pwd -P)
release_commit=$(git rev-parse --verify "refs/tags/$release_version^{commit}")
[[ $(git rev-parse HEAD) == "$release_commit" ]] || exit 1
validate_assets() {
  python3 - "$1" "$release_version" "$release_commit" <<'PY'
import hashlib,pathlib,re,sys
p=pathlib.Path(sys.argv[1]); version,commit=sys.argv[2:]
names={'install.sh','install.ps1','SOURCE-COMMIT.txt'}
names.update('clawbridge_'+version+'_'+target for target in ['darwin_amd64','darwin_arm64','linux_amd64','linux_arm64','windows_amd64.exe'])
assert {f.name for f in p.iterdir()}==names|{'SHA256SUMS'},'unexpected/missing assets'
assert all(f.is_file() and not f.is_symlink() and f.stat().st_size for f in p.iterdir()),'nonregular/empty asset'
assert (p/'SOURCE-COMMIT.txt').read_text()==commit+'\n','source marker mismatch'
seen={}
for line in (p/'SHA256SUMS').read_text().splitlines():
 m=re.fullmatch(r'([0-9a-f]{64})  ([A-Za-z0-9_.-]+)',line)
 assert m and m[2] not in seen,'malformed/duplicate manifest'
 seen[m[2]]=m[1]
assert set(seen)==names,'manifest coverage mismatch'
for name,digest in seen.items(): assert hashlib.sha256((p/name).read_bytes()).hexdigest()==digest,'hash mismatch: '+name
PY
}
validate_assets "$release_assets"
release_verify=$(mktemp -d)
trap 'rm -rf -- "$release_verify"' EXIT
# Notes come from the tagged tree, never conversation/history or a mutable file.
git show "$release_commit:RELEASE-NOTES.md" > "$release_verify/notes.md"
# Listing drafts as well as published releases distinguishes absence from an API
# failure and refuses existing drafts, too. A failed query stops before creation.
release_existing=$(gh api --paginate 'repos/jkai-chen/ClawBridge/releases?per_page=100' --jq ".[] | select(.tag_name == \"$release_version\") | .id")
[[ -z $release_existing ]] || { echo 'release already exists; root must recover it explicitly' >&2; exit 1; }
gh release create "$release_version" --verify-tag --draft --title "$release_version" --notes-file "$release_verify/notes.md"
gh release upload "$release_version" "$release_assets/"*
mkdir "$release_verify/download"
gh release download "$release_version" --dir "$release_verify/download"
validate_assets "$release_verify/download"
for release_file in "$release_assets/"*; do
  cmp "$release_file" "$release_verify/download/${release_file##*/}"
done
[[ $(gh release view "$release_version" --json isDraft,tagName --jq '.isDraft') == true ]] || { echo 'expected draft before publication' >&2; exit 1; }
gh release edit "$release_version" --draft=false --latest
