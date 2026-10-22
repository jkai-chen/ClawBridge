"""Isolated tagged-clone release builder probe. Never changes the source checkout."""
import json
import platform
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

source = Path(sys.argv[1]).resolve()
env = os.environ.copy()
env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0", GIT_AUTHOR_NAME="Release Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid", GIT_COMMITTER_NAME="Release Fixture", GIT_COMMITTER_EMAIL="fixture@example.invalid", GIT_AUTHOR_DATE="2026-10-21T00:00:00+00:00", GIT_COMMITTER_DATE="2026-10-21T00:00:00+00:00", GOTOOLCHAIN="local")

def check(condition, detail="release builder probe check failed"):
    # Explicit failure, never an assert, so `python3 -O` cannot skip a check.
    if not condition:
        raise SystemExit("FAIL: " + repr(detail))

def run(args, cwd, ok=True, extra=None):
    actual_env = env.copy()
    if extra:
        actual_env.update(extra)
    r = subprocess.run(args, cwd=cwd, env=actual_env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=240)
    check((r.returncode == 0) == ok, (args, r.returncode, r.stdout.decode(errors="replace")))
    return r.stdout

with tempfile.TemporaryDirectory(prefix="cb-release-clone-") as temporary:
    root = Path(temporary)
    clone = root / "source with spaces"
    run(["git", "-c", "core.hooksPath="+os.devnull, "clone", "--quiet", "--no-hardlinks", "--no-local", str(source), str(clone)], source)
    # Snapshot current frozen code atop the clone, then create fixture-only commit/tag.
    names = run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], source).decode().split("\0")
    for name in set(names)-{""}:
        original = source/name
        destination = clone/name
        if original.is_file():
            check(not original.is_symlink(), "fixture refuses symlink source")
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(original, destination)
        elif not original.exists() and destination.is_file():
            destination.unlink()
    def git(*args):
        return run(["git", "-c", "core.hooksPath="+os.devnull, "-c", "commit.gpgsign=false", *args], clone)
    git("add", "-A")
    git("commit", "--quiet", "--allow-empty", "-m", "isolated release acceptance fixture")
    git("tag", "v1.2.3")
    git("tag", "v9.9.9", "HEAD^")
    commit = git("rev-parse", "HEAD").decode().strip()
    builder = str(clone/"scripts/build-release.sh")
    output = root/"release output"
    for version in ["v01.2.3", "v1.2.3-rc1", "v1.2.3;echo-unsafe", "v9.9.9", "v8.8.8"]:
        run(["bash", builder, version, str(output)], clone, ok=False)
        check(not output.exists())
    tracked = clone/"go.mod"
    old = tracked.read_bytes()
    tracked.write_bytes(old+b"\n")
    run(["bash", builder, "v1.2.3", str(output)], clone, ok=False)
    tracked.write_bytes(old)
    untracked = clone/"untracked-release-input"
    untracked.write_text("must reject\n")
    run(["bash", builder, "v1.2.3", str(output)], clone, ok=False)
    untracked.unlink()
    run(["bash", builder, "v1.2.3", str(clone/"not-ignored-output")], clone, ok=False)
    check(not (clone/"not-ignored-output").exists())
    output.mkdir()
    (output/"existing-payload").write_bytes(b"preserve")
    run(["bash", builder, "v1.2.3", str(output)], clone, ok=False)
    check((output/"existing-payload").read_bytes()==b"preserve")
    (output/"existing-payload").unlink()
    run(["bash", builder, "v1.2.3", str(output)], clone, extra={"GOOS":"windows", "GOARCH":"386", "CGO_ENABLED":"1", "GOAMD64":"v4"})
    targets = [("darwin","amd64"),("darwin","arm64"),("linux","amd64"),("linux","arm64"),("windows","amd64")]
    assets = {"clawbridge_v1.2.3_"+system+"_"+arch+(".exe" if system=="windows" else "") for system,arch in targets}
    check({p.name for p in output.iterdir()}==assets|{"install.sh","install.ps1","LICENSE","SOURCE-COMMIT.txt","SHA256SUMS"})
    check((output/"SOURCE-COMMIT.txt").read_text()==commit+"\n")
    for installer in ["install.sh","install.ps1"]:
        check((output/installer).read_bytes()==(clone/"scripts"/installer).read_bytes())
    # The shipped licence is the tagged source's own LICENSE, byte for byte.
    check((output/"LICENSE").read_bytes()==(clone/"LICENSE").read_bytes())
    expected = "".join(hashlib.sha256(p.read_bytes()).hexdigest()+"  "+p.name+"\n" for p in sorted(output.iterdir()) if p.name!="SHA256SUMS")
    check((output/"SHA256SUMS").read_text()==expected)
    for system,arch in targets:
        binary = output/("clawbridge_v1.2.3_"+system+"_"+arch+(".exe" if system=="windows" else ""))
        metadata = run(["go","version","-m",str(binary)], clone).decode()
        for value in ["CGO_ENABLED=0", "GOOS="+system, "GOARCH="+arch, "-trimpath=true"]:
            check(value in metadata, (binary.name, value))
        if "vcs.revision=" in metadata:
            check("vcs.revision="+commit in metadata and "vcs.modified=false" in metadata, binary.name)
        content = binary.read_bytes()
        for embedded in ["v1.2.3", commit, "2026-10-21T00:00:00Z"]:
            check(embedded.encode() in content, (binary.name, "missing embedded release value", embedded))
        header = content[:4]
        if system=="linux":
            check(header==b"\x7fELF")
        elif system=="darwin":
            check(header==b"\xcf\xfa\xed\xfe")
        else:
            check(header[:2]==b"MZ")
    # Go 1.27 trimpath metadata may omit literal linker flags; native output
    # verifies injection independently of that optional metadata representation.
    native_os = {"darwin":"darwin", "linux":"linux"}.get(sys.platform)
    native_arch = {"x86_64":"amd64", "AMD64":"amd64", "arm64":"arm64", "aarch64":"arm64"}.get(platform.machine())
    check((native_os,native_arch) in targets, "no supported native version fixture")
    native_binary = output/("clawbridge_v1.2.3_"+native_os+"_"+native_arch)
    isolated_home = root/"native isolated home"
    isolated_home.mkdir()
    poison = isolated_home/"state-is-file"
    poison.write_text("malformed = [\n")
    version_env = {"HOME":str(isolated_home), "CLAWBRIDGE_HOME":str(poison), "CLAWBRIDGE_MODE":"invalid-runtime-mode", "PATH":os.defpath}
    result = subprocess.run([str(native_binary),"version"], cwd=root, env=version_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
    check(result.returncode==0, result.stderr.decode(errors="replace"))
    check(json.loads(result.stdout)=={"source":"build","data":{"version":"v1.2.3","commit":commit,"build_time":"2026-10-21T00:00:00Z"}})
    check(list(isolated_home.iterdir())==[poison], "native version changed isolated state")
    check(git("status","--porcelain").strip()==b"", "builder changed clone source")
    run(["bash",builder,"v1.2.3",str(output)], clone, ok=False)
    check((output/"SHA256SUMS").read_text()==expected)
    print("PASS isolated clone/tag: 5 real targets, exact metadata/9 assets/8 digests, canonical+matchingtag+dirty+output refusal; native version only; builder executes no targets")
