#!/usr/bin/env bash
# CI-only publication, after independent tests. Never replaces a release/asset.
set -euo pipefail
[[ $# == 2 ]] || { echo 'usage: publish-release.sh VERSION ASSET_DIRECTORY' >&2; exit 1; }
release_version=$1
[[ $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || exit 1
[[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF:-} == "refs/tags/$release_version" && ${GITHUB_REPOSITORY:-} == jkai-chen/ClawBridge ]] || { echo 'publication requires the exact repository tag-push CI context' >&2; exit 1; }
# The server image this release names, pushed and attested by the workflow
# before this script runs.
[[ ${IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'publication requires the pushed image digest (IMAGE_DIGEST)' >&2; exit 1; }
release_image="ghcr.io/jkai-chen/clawbridge@$IMAGE_DIGEST"
export GH_REPO=jkai-chen/ClawBridge
# The Sigstore bundle of the assets' attestation (FR24): published as an asset
# so a client verifies provenance with no API call and no token.
[[ -f ${PROVENANCE_BUNDLE:-} && ! -L ${PROVENANCE_BUNDLE:-} ]] || { echo 'publication requires the assets attestation bundle (PROVENANCE_BUNDLE)' >&2; exit 1; }
release_assets=$(cd "$2" && pwd -P)
release_commit=$(git rev-parse --verify "refs/tags/$release_version^{commit}")
[[ $(git rev-parse HEAD) == "$release_commit" ]] || exit 1
# validate_assets DIR EXTRAS: DIR holds exactly the attested assets, SHA256SUMS
# and EXTRAS (space-separated names outside SHA256SUMS; each checked below).
validate_assets() {
  python3 - "$1" "$release_version" "$release_commit" "$release_image" "$2" <<'PY'
import base64,hashlib,json,pathlib,re,sys
p=pathlib.Path(sys.argv[1]); version,commit,image,extras=sys.argv[2:]
extras=set(extras.split())
names={'LICENSE','install.sh','install.ps1','SOURCE-COMMIT.txt','IMAGE-DIGEST.txt'}
names.update('clawbridge_'+version+'_'+target for target in ['darwin_amd64','darwin_arm64','linux_amd64','linux_arm64','windows_amd64.exe','windows_arm64.exe'])
assert extras<={'PROVENANCE.sigstore.json','MANUAL-UPDATE-REQUIRED'},'unknown extra asset'
assert {f.name for f in p.iterdir()}==names|{'SHA256SUMS'}|extras,'unexpected/missing assets'
assert all(f.is_file() and not f.is_symlink() and f.stat().st_size for f in p.iterdir()),'nonregular/empty asset'
assert (p/'SOURCE-COMMIT.txt').read_text()==commit+'\n','source marker mismatch'
assert (p/'IMAGE-DIGEST.txt').read_text()==image+'\n','image digest mismatch'
seen={}
for line in (p/'SHA256SUMS').read_text().splitlines():
 m=re.fullmatch(r'([0-9a-f]{64})  ([A-Za-z0-9_.-]+)',line)
 assert m and m[2] not in seen,'malformed/duplicate manifest'
 seen[m[2]]=m[1]
assert set(seen)==names,'manifest coverage mismatch'
for name,digest in seen.items(): assert hashlib.sha256((p/name).read_bytes()).hexdigest()==digest,'hash mismatch: '+name
if 'PROVENANCE.sigstore.json' in extras:
 # The bundle attests exactly the assets above and SHA256SUMS, by digest:
 # a client checks its binary against it with no other source.
 b=json.loads((p/'PROVENANCE.sigstore.json').read_text())
 assert b.get('mediaType','').startswith('application/vnd.dev.sigstore.bundle'),'not a Sigstore bundle'
 env=b['dsseEnvelope']; assert env['payloadType']=='application/vnd.in-toto+json' and env['signatures'],'not a signed in-toto envelope'
 st=json.loads(base64.b64decode(env['payload']))
 assert st['predicateType']=='https://slsa.dev/provenance/v1','not SLSA provenance'
 got={s['name']:s['digest']['sha256'] for s in st['subject']}
 want=dict(seen); want['SHA256SUMS']=hashlib.sha256((p/'SHA256SUMS').read_bytes()).hexdigest()
 assert got==want,'bundle subjects are not the assets'
if 'MANUAL-UPDATE-REQUIRED' in extras:
 assert (p/'MANUAL-UPDATE-REQUIRED').read_text().startswith(version+' needs a manual update'),'manual-update marker text'
PY
}
validate_assets "$release_assets" ""
release_verify=$(mktemp -d)
trap 'rm -rf -- "$release_verify"' EXIT
# Notes come from the tagged tree, never conversation/history or a mutable file.
# The body (scripts/release_body.py) is this release's own top section, a link
# to the full notes at the tag, and the image the workflow pushed: the image
# digest can't be in the tagged notes, which are part of what was built. A body
# GitHub would refuse fails here, before anything is created.
git show "$release_commit:RELEASE-NOTES.md" > "$release_verify/all-notes.md"
python3 "$(dirname "$0")/release_body.py" "$release_verify/all-notes.md" "$release_verify/notes.md" "$release_version" "$release_image"
# What is uploaded: the attested assets, the bundle, and the manual-update
# marker when the release's notes carry one (release_body.py --manual), which
# auto-updates refuse to install (FR24).
release_upload="$release_verify/upload"
mkdir "$release_upload"
cp -p "$release_assets/"* "$release_upload/"
cp "$PROVENANCE_BUNDLE" "$release_upload/PROVENANCE.sigstore.json"
release_extras=PROVENANCE.sigstore.json
# Exit 0: marker written; 3: the notes carry none; anything else stops here.
release_manual=0
python3 "$(dirname "$0")/release_body.py" --manual "$release_verify/all-notes.md" "$release_upload/MANUAL-UPDATE-REQUIRED" "$release_version" || release_manual=$?
case $release_manual in
  0) release_extras="$release_extras MANUAL-UPDATE-REQUIRED" ;;
  3) ;;
  *) echo 'could not read the manual-update marker; nothing was created' >&2; exit 1 ;;
esac
validate_assets "$release_upload" "$release_extras"
# Listing drafts as well as published releases distinguishes absence from an API
# failure and refuses existing drafts, too. A failed query stops before creation.
release_existing=$(gh api --paginate 'repos/jkai-chen/ClawBridge/releases?per_page=100' --jq ".[] | select(.tag_name == \"$release_version\") | .id")
[[ -z $release_existing ]] || { echo 'release already exists; root must recover it explicitly' >&2; exit 1; }
gh release create "$release_version" --verify-tag --draft --title "$release_version" --notes-file "$release_verify/notes.md"
gh release upload "$release_version" "$release_upload/"*
mkdir "$release_verify/download"
gh release download "$release_version" --dir "$release_verify/download"
validate_assets "$release_verify/download" "$release_extras"
for release_file in "$release_upload/"*; do
  cmp "$release_file" "$release_verify/download/${release_file##*/}"
done
[[ $(gh release view "$release_version" --json isDraft,tagName --jq '.isDraft') == true ]] || { echo 'expected draft before publication' >&2; exit 1; }
# Only the highest stable version becomes Latest, so a backport (v0.1.4 after
# v0.2.0) never redirects releases/latest. Drafts, prereleases and noncanonical
# tags are ignored. A failed query stops here and leaves the verified draft.
release_latest=$(gh api --paginate 'repos/jkai-chen/ClawBridge/releases?per_page=100' --jq '.[] | {tag_name, draft, prerelease}' | python3 -c '
import json,re,sys
def version(tag):
 m=re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)",tag)
 return tuple(map(int,m.groups())) if m else None
mine=version(sys.argv[1])
for line in sys.stdin:
 release=json.loads(line)
 if release["draft"] or release["prerelease"]: continue
 other=version(release["tag_name"])
 if other is not None and other>mine: print("false"); break
else: print("true")
' "$release_version")
[[ $release_latest == true || $release_latest == false ]] || { echo 'could not decide the latest release' >&2; exit 1; }
gh release edit "$release_version" --draft=false --latest="$release_latest"
