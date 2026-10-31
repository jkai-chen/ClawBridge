#!/usr/bin/env python3
"""The GitHub release body: the release's own top section of RELEASE-NOTES.md,
a link to the full notes at the tag, the server image, and the two images
agentd deploys.

publish-release.sh writes the body with it, and test_publish.py checks the
current RELEASE-NOTES.md with it in every PR and main CI run, so a body too long
for GitHub fails long before a tag.

    release_body.py NOTES OUT VERSION IMAGE [AGENTD_IMAGE AGENT_IMAGE]
        write the body, or exit non-zero; refused unless the top heading is
        "# ClawBridge VERSION (released ...)"
    release_body.py --manual NOTES OUT VERSION
        write the MANUAL-UPDATE-REQUIRED asset when the release's top section
        carries the marker line (exit 0), or exit 3 when it carries none
"""
import pathlib
import re
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


# A release whose top section has a line starting with this marker needs a
# manual update: automatic updates (FR24) never install it. Release authors
# add it, with the reason, for a breaking client change such as a sign-in
# method going away.
MANUAL_MARKER = "**Manual update required.**"


def manual_line(notes):
    """The marker line of the release's top section, outside code fences, or
    None."""
    fence = False
    for line in top_section(notes).splitlines():
        if line.startswith("```"):
            fence = not fence
        elif not fence and line.startswith(MANUAL_MARKER):
            return line.strip()
    return None


def manual_asset(line, version):
    """The MANUAL-UPDATE-REQUIRED asset's text: plain, short, the reason."""
    return version + " needs a manual update (clawbridge self-update); see its release notes.\n" + line[:2000] + "\n"


# The top heading a release's notes must have at its tag: renaming
# "# Unreleased" is the cut's step, and a release whose body would start with
# it, or with another release's heading, is refused before anything is
# created.
HEADING = re.compile(r"# ClawBridge (v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)) \(released [^()\n]+\)")


def heading_error(notes, version):
    """None when the first line names this release, else why not."""
    first = notes.split("\n", 1)[0].rstrip("\r")
    m = HEADING.fullmatch(first)
    if m is None:
        return 'the top heading of RELEASE-NOTES.md is %r, not "# ClawBridge %s (released DATE)": rename it at the cut' % (first[:80], version)
    if m.group(1) != version:
        return "the top heading of RELEASE-NOTES.md is for %s, not %s" % (m.group(1), version)
    return None


def verify_line(image):
    return "`gh attestation verify oci://" + image + " --repo jkai-chen/ClawBridge --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml`"


def release_body(notes, version, image, agentd_image=None, agent_image=None):
    body = top_section(notes).rstrip("\n") + "\n\n"
    body += "The full release notes: https://github.com/jkai-chen/ClawBridge/blob/" + version + "/RELEASE-NOTES.md\n\n"
    body += "## Server image\n\n`" + image + "` (linux/amd64, linux/arm64). Pin it by this digest; check it with\n"
    body += verify_line(image) + ".\n"
    # The images agentd deploys (docs/operations/agentd.md), pinned and
    # checked the same way; their digests are also the release's
    # AGENTD-IMAGE-DIGEST.txt and AGENT-IMAGE-DIGEST.txt assets.
    if agentd_image and agent_image:
        body += "\n## Agent images\n\nFor agentd (linux/amd64, linux/arm64), pinned by digest and checked the same way:\n\n"
        body += "- agentd and its egress container: `" + agentd_image + "`; " + verify_line(agentd_image) + "\n"
        body += "- the dispatcher agent: `" + agent_image + "`; " + verify_line(agent_image) + "\n"
    return body


def check(body):
    """The body, or a ValueError naming its length when GitHub would refuse it."""
    if len(body) > MAX_BODY:
        raise ValueError("release body is %d characters, over the %d limit: shorten the release's top section of RELEASE-NOTES.md" % (len(body), MAX_BODY))
    return body


def main(argv):
    if len(argv) == 5 and argv[1] == "--manual":
        notes, out, version = argv[2:]
        line = manual_line(pathlib.Path(notes).read_text(encoding="utf-8"))
        if line is None:
            sys.exit(3)
        pathlib.Path(out).write_text(manual_asset(line, version), encoding="utf-8")
        return
    if len(argv) not in (5, 7):
        sys.exit(__doc__)
    notes, out, version, image = argv[1:5]
    agents = argv[5:7]
    text = pathlib.Path(notes).read_text(encoding="utf-8")
    error = heading_error(text, version)
    if error is not None:
        sys.exit(error + "; nothing was created")
    try:
        body = check(release_body(text, version, image, *agents))
    except ValueError as e:
        sys.exit(str(e) + "; nothing was created")
    pathlib.Path(out).write_text(body, encoding="utf-8")


if __name__ == "__main__":
    main(sys.argv)
