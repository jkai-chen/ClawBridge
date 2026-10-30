#!/usr/bin/env python3
"""The GitHub release body: the release's own top section of RELEASE-NOTES.md,
a link to the full notes at the tag, and the server image.

publish-release.sh writes the body with it, and test_publish.py checks the
current RELEASE-NOTES.md with it in every PR and main CI run, so a body too long
for GitHub fails long before a tag.

    release_body.py NOTES OUT VERSION IMAGE   write the body, or exit non-zero
"""
import pathlib
import sys

# GitHub refuses a release body of 125000 characters or more; this leaves room.
MAX_BODY = 120000


def top_section(notes):
    """Line 1 up to, not including, the next top-level "# " heading outside a
    code fence."""
    lines = notes.splitlines(True)
    top, fence = [], False
    for i, line in enumerate(lines):
        if line.startswith("```"):
            fence = not fence
        if i > 0 and not fence and line.startswith("# "):
            break
        top.append(line)
    return "".join(top)


def release_body(notes, version, image):
    body = top_section(notes).rstrip("\n") + "\n\n"
    body += "The full release notes: https://github.com/jkai-chen/ClawBridge/blob/" + version + "/RELEASE-NOTES.md\n\n"
    body += "## Server image\n\n`" + image + "` (linux/amd64, linux/arm64). Pin it by this digest; check it with\n"
    body += "`gh attestation verify oci://" + image + " --repo jkai-chen/ClawBridge --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml`.\n"
    return body


def check(body):
    """The body, or a ValueError naming its length when GitHub would refuse it."""
    if len(body) > MAX_BODY:
        raise ValueError("release body is %d characters, over the %d limit: shorten the release's top section of RELEASE-NOTES.md" % (len(body), MAX_BODY))
    return body


def main(argv):
    if len(argv) != 5:
        sys.exit(__doc__)
    notes, out, version, image = argv[1:]
    try:
        body = check(release_body(pathlib.Path(notes).read_text(encoding="utf-8"), version, image))
    except ValueError as e:
        sys.exit(str(e) + "; nothing was created")
    pathlib.Path(out).write_text(body, encoding="utf-8")


if __name__ == "__main__":
    main(sys.argv)
