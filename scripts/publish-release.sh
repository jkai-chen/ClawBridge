#!/usr/bin/env bash
# CI-only publication, after independent tests. Never replaces a release/asset.
#
#   publish-release.sh VERSION ASSET_DIRECTORY
#       the tag-push run (release.yml): draft, upload, verify, then publish, or
#       stop at the verified draft when RELEASE_HOLD_DRAFT is true
#   publish-release.sh --publish-draft VERSION
#       the dispatch run (publish-draft.yml): validate a held draft again from
#       its own assets, then publish it, with no rebuild and no upload
set -euo pipefail
if [[ $# == 2 && $1 == --publish-draft ]]; then
  release_mode=publish-draft
  release_version=$2
elif [[ $# == 2 ]]; then
  release_mode=release
  release_version=$1
else
  echo 'usage: publish-release.sh VERSION ASSET_DIRECTORY | --publish-draft VERSION' >&2; exit 1
fi
[[ $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || exit 1
# The repository variable RELEASE_HOLD_DRAFT (docs/design/releases.md): true
# stops a tag-push run at the verified draft. A typo must not publish.
case ${RELEASE_HOLD_DRAFT:-} in
  true|false|'') ;;
  *) echo 'RELEASE_HOLD_DRAFT must be true, false or empty' >&2; exit 1 ;;
esac
export GH_REPO=jkai-chen/ClawBridge
if [[ $release_mode == release ]]; then
  [[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF:-} == "refs/tags/$release_version" && ${GITHUB_REPOSITORY:-} == jkai-chen/ClawBridge ]] || { echo 'publication requires the exact repository tag-push CI context' >&2; exit 1; }
  # The server image this release names, pushed and attested by the workflow
  # before this script runs.
  [[ ${IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'publication requires the pushed image digest (IMAGE_DIGEST)' >&2; exit 1; }
  release_image="ghcr.io/jkai-chen/clawbridge@$IMAGE_DIGEST"
  # The agentd (egress) and dispatcher agent images, pushed and attested the
  # same way: a deploy pins all three from the release's assets.
  [[ ${AGENTD_IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'publication requires the pushed agentd image digest (AGENTD_IMAGE_DIGEST)' >&2; exit 1; }
  release_agentd_image="ghcr.io/jkai-chen/clawbridge-agentd@$AGENTD_IMAGE_DIGEST"
  [[ ${AGENT_IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'publication requires the pushed dispatcher agent image digest (AGENT_IMAGE_DIGEST)' >&2; exit 1; }
  release_agent_image="ghcr.io/jkai-chen/clawbridge-dispatcher-agent@$AGENT_IMAGE_DIGEST"
  # The Sigstore bundle of the assets' attestation (FR24): published as an asset
  # so a client verifies provenance with no API call and no token.
  [[ -f ${PROVENANCE_BUNDLE:-} && ! -L ${PROVENANCE_BUNDLE:-} ]] || { echo 'publication requires the assets attestation bundle (PROVENANCE_BUNDLE)' >&2; exit 1; }
  release_assets=$(cd "$2" && pwd -P)
else
  # A dispatch from the tag itself (gh workflow run --ref VERSION), so the
  # workflow file that publishes is the one the owner tagged, as for a tag push.
  [[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${GITHUB_REF:-} == "refs/tags/$release_version" && ${GITHUB_REPOSITORY:-} == jkai-chen/ClawBridge ]] || { echo 'publishing a draft requires the exact repository dispatch-from-tag CI context' >&2; exit 1; }
fi
release_commit=$(git rev-parse --verify "refs/tags/$release_version^{commit}")
[[ $(git rev-parse HEAD) == "$release_commit" ]] || { echo 'the checkout is not the tagged commit' >&2; exit 1; }
# validate_assets DIR EXTRAS: DIR holds exactly the attested assets, SHA256SUMS
# and EXTRAS (space-separated names outside SHA256SUMS; each checked below).
validate_assets() {
  python3 - "$1" "$release_version" "$release_commit" "$release_image" "$release_agentd_image" "$release_agent_image" "$2" <<'PY'
import base64,hashlib,json,pathlib,re,sys
p=pathlib.Path(sys.argv[1]); version,commit,image,agentd_image,agent_image,extras=sys.argv[2:]
extras=set(extras.split())
names={'LICENSE','install.sh','install.ps1','SOURCE-COMMIT.txt','IMAGE-DIGEST.txt','AGENTD-IMAGE-DIGEST.txt','AGENT-IMAGE-DIGEST.txt'}
names.update('clawbridge_'+version+'_'+target for target in ['darwin_amd64','darwin_arm64','linux_amd64','linux_arm64','windows_amd64.exe','windows_arm64.exe'])
assert extras<={'PROVENANCE.sigstore.json','MANUAL-UPDATE-REQUIRED'},'unknown extra asset'
assert {f.name for f in p.iterdir()}==names|{'SHA256SUMS'}|extras,'unexpected/missing assets'
assert all(f.is_file() and not f.is_symlink() and f.stat().st_size for f in p.iterdir()),'nonregular/empty asset'
assert (p/'SOURCE-COMMIT.txt').read_text()==commit+'\n','source marker mismatch'
assert (p/'IMAGE-DIGEST.txt').read_text()==image+'\n','image digest mismatch'
assert (p/'AGENTD-IMAGE-DIGEST.txt').read_text()==agentd_image+'\n','agentd image digest mismatch'
assert (p/'AGENT-IMAGE-DIGEST.txt').read_text()==agent_image+'\n','dispatcher agent image digest mismatch'
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
# publish_release [NOTES]: mark the verified draft published, with NOTES as its
# body when given (publish-draft sets the body it validated). Only the highest stable
# version becomes Latest, so a backport (v0.1.4 after v0.2.0) never redirects
# releases/latest. Drafts, prereleases and noncanonical tags are ignored. A
# failed query stops here and leaves the verified draft.
publish_release() {
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
  if [[ $# == 1 ]]; then
    gh release edit "$release_version" --draft=false --latest="$release_latest" --notes-file "$1"
  else
    gh release edit "$release_version" --draft=false --latest="$release_latest"
  fi
}
release_verify=$(mktemp -d)
trap 'rm -rf -- "$release_verify"' EXIT
# Notes come from the tagged tree, never conversation/history or a mutable file.
git show "$release_commit:RELEASE-NOTES.md" > "$release_verify/all-notes.md"
if [[ $release_mode == publish-draft ]]; then
  # Only a draft of exactly this tag is published; a published release, a
  # missing one or another tag's stops here. A draft's target is the default
  # branch's name unless it was created with a commit: a commit must be the
  # tag's.
  release_state=$(gh release view "$release_version" --json isDraft,tagName,targetCommitish --jq '"\(.isDraft) \(.tagName) \(.targetCommitish)"') || { echo "no release $release_version to publish" >&2; exit 1; }
  read -r release_draft release_tag release_target <<<"$release_state"
  [[ $release_draft == true ]] || { echo "release $release_version is not a draft; nothing was changed" >&2; exit 1; }
  [[ $release_tag == "$release_version" ]] || { echo "the draft is for $release_tag, not $release_version" >&2; exit 1; }
  [[ ! $release_target =~ ^[0-9a-f]{40}$ || $release_target == "$release_commit" ]] || { echo "the draft targets $release_target, not the tagged commit" >&2; exit 1; }
  # Nothing local is trusted: the draft's own assets are validated as the
  # tag-push run validated them, against the image named in its
  # IMAGE-DIGEST.txt and the marker the tagged notes call for.
  mkdir "$release_verify/download"
  gh release download "$release_version" --dir "$release_verify/download"
  [[ -f $release_verify/download/IMAGE-DIGEST.txt ]] || { echo 'the draft has no IMAGE-DIGEST.txt' >&2; exit 1; }
  release_image=$(head -c 200 "$release_verify/download/IMAGE-DIGEST.txt")
  [[ $release_image =~ ^ghcr\.io/jkai-chen/clawbridge@sha256:[0-9a-f]{64}$ ]] || { echo "the draft's IMAGE-DIGEST.txt does not name a ClawBridge image digest" >&2; exit 1; }
  # And the two agent images, from their own digest assets.
  [[ -f $release_verify/download/AGENTD-IMAGE-DIGEST.txt && -f $release_verify/download/AGENT-IMAGE-DIGEST.txt ]] || { echo 'the draft has no AGENTD-IMAGE-DIGEST.txt or AGENT-IMAGE-DIGEST.txt' >&2; exit 1; }
  release_agentd_image=$(head -c 200 "$release_verify/download/AGENTD-IMAGE-DIGEST.txt")
  [[ $release_agentd_image =~ ^ghcr\.io/jkai-chen/clawbridge-agentd@sha256:[0-9a-f]{64}$ ]] || { echo "the draft's AGENTD-IMAGE-DIGEST.txt does not name an agentd image digest" >&2; exit 1; }
  release_agent_image=$(head -c 200 "$release_verify/download/AGENT-IMAGE-DIGEST.txt")
  [[ $release_agent_image =~ ^ghcr\.io/jkai-chen/clawbridge-dispatcher-agent@sha256:[0-9a-f]{64}$ ]] || { echo "the draft's AGENT-IMAGE-DIGEST.txt does not name a dispatcher agent image digest" >&2; exit 1; }
  release_extras=PROVENANCE.sigstore.json
  release_manual=0
  python3 "$(dirname "$0")/release_body.py" --manual "$release_verify/all-notes.md" "$release_verify/MANUAL-UPDATE-REQUIRED" "$release_version" || release_manual=$?
  case $release_manual in
    0) release_extras="$release_extras MANUAL-UPDATE-REQUIRED" ;;
    3) ;;
    *) echo 'could not read the manual-update marker; nothing was changed' >&2; exit 1 ;;
  esac
  validate_assets "$release_verify/download" "$release_extras"
  if [[ $release_manual == 0 ]]; then
    cmp "$release_verify/MANUAL-UPDATE-REQUIRED" "$release_verify/download/MANUAL-UPDATE-REQUIRED"
  fi
  # validate_assets checks the bundle's subjects, not who signed it: anyone who
  # can edit the draft could forge one. Its signature must be release.yml's,
  # run for this tag at the tagged commit on a GitHub-hosted runner.
  # SHA256SUMS is one of its subjects (attest-assets attests every asset and
  # SHA256SUMS), and validate_assets ties every asset to SHA256SUMS.
  gh attestation verify "$release_verify/download/SHA256SUMS" --bundle "$release_verify/download/PROVENANCE.sigstore.json" --repo jkai-chen/ClawBridge --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref "refs/tags/$release_version" --source-digest "$release_commit" --predicate-type https://slsa.dev/provenance/v1 --deny-self-hosted-runners || { echo "the draft's provenance does not verify; nothing was changed" >&2; exit 1; }
  # The body is set again as the tag-push run wrote it, from the tagged notes
  # and the draft's verified image, so an edited draft body is not published.
  python3 "$(dirname "$0")/release_body.py" "$release_verify/all-notes.md" "$release_verify/notes.md" "$release_version" "$release_image" "$release_agentd_image" "$release_agent_image"
  publish_release "$release_verify/notes.md"
  echo "published $release_version; image $release_image"
  exit 0
fi
validate_assets "$release_assets" ""
# The body (scripts/release_body.py) is this release's own top section, a link
# to the full notes at the tag, and the image the workflow pushed: the image
# digest can't be in the tagged notes, which are part of what was built. A body
# GitHub would refuse fails here, before anything is created.
python3 "$(dirname "$0")/release_body.py" "$release_verify/all-notes.md" "$release_verify/notes.md" "$release_version" "$release_image" "$release_agentd_image" "$release_agent_image"
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
# The bundle's signature, as a client checks it: Sigstore's chain and log, the
# release workflow at this tag and commit on a GitHub-hosted runner, SLSA v1,
# and one asset's digest: the identity publish-draft demands too. A bundle that
# would not verify stops here, before anything is created.
gh attestation verify "$release_upload/clawbridge_${release_version}_linux_amd64" --bundle "$release_upload/PROVENANCE.sigstore.json" --repo jkai-chen/ClawBridge --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref "refs/tags/$release_version" --source-digest "$release_commit" --predicate-type https://slsa.dev/provenance/v1 --deny-self-hosted-runners || { echo 'the provenance bundle does not verify; nothing was created' >&2; exit 1; }
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
# The hold (RELEASE_HOLD_DRAFT=true): the run ends at the verified draft, whose
# image is deployed and soaked; publish-draft.yml publishes these same bytes.
if [[ ${RELEASE_HOLD_DRAFT:-} == true ]]; then
  echo "held as draft: $release_version; image $release_image; publish with the publish-draft workflow"
  exit 0
fi
publish_release
