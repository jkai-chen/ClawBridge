#!/usr/bin/env python3
"""Offline, isolated old-source serializer -> actual current CLI replay probe.

Requires the named historical commit to exist locally; never fetches history.
The original checked-in ledger fixtures are not rewritten or substituted.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile

SOURCE = "8aef0f97802169efdf6895eb84bf468a3a9f05c8"
ROOT = Path(__file__).resolve().parents[2]


def sha(data):
    return hashlib.sha256(data).hexdigest()


def files(root):
    return {str(p.relative_to(root)): sha(p.read_bytes()) for p in sorted(root.rglob("*")) if p.is_file()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    opts = parser.parse_args()
    out = opts.output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    env = dict(os.environ, GOENV="off", GOWORK="off", GOFLAGS="", GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
    records = []

    def run(label, argv, cwd, runenv=env, okay=True):
        p = subprocess.run(argv, cwd=cwd, env=runenv, capture_output=True, timeout=120)
        (out / (label + ".stdout")).write_bytes(p.stdout)
        (out / (label + ".stderr")).write_bytes(p.stderr)
        records.append({"label": label, "argv": [str(x) for x in argv], "exit": p.returncode})
        if okay and p.returncode:
            raise RuntimeError(label + " failed; inspect private stdout/stderr")
        return p

    scratch = Path(tempfile.mkdtemp(prefix="cb-l2-cli-old-source-"))
    try:
        archive = run("archive", ["git", "archive", SOURCE], ROOT).stdout
        (out / "source.tar").write_bytes(archive)
        old = scratch / "old-source"
        old.mkdir()
        with tarfile.open(out / "source.tar") as tar:
            for member in tar.getmembers():
                parts = Path(member.name).parts
                if Path(member.name).is_absolute() or ".." in parts or not (member.isfile() or member.isdir()):
                    raise RuntimeError("unsafe source archive member")
            tar.extractall(old, filter="data")
        original = files(old)
        fixture = out / "old generated fixture"
        db = fixture / "schema3.db"
        # Independently reproduce the unchanged L1 CLI authority route format;
        # the ACTUAL old serializer hashes this value, never the current code.
        authority = sha(json.dumps(["local", str(db.resolve())], separators=(",", ":")).encode())
        generator = (ROOT / "integration/testdata/linear-l1-v3-generator.go.txt").read_text()
        old_constant = 'const authority = "synthetic:actual-l1-schema3-ledger-authority"'
        assert generator.count(old_constant) == 1
        generator = generator.replace(old_constant, "const authority = " + json.dumps(authority))
        helper = old / "cmd/l2-legacy-cli-fixture/main.go"
        helper.parent.mkdir()
        helper.write_text(generator)
        (out / "runtime-generator.go.txt").write_text(generator)
        old_bin, new_bin = scratch / "old-generator", scratch / "current-clawbridge"
        run("build-old", ["go", "build", "-o", str(old_bin), "./cmd/l2-legacy-cli-fixture"], old)
        run("generate-old", [str(old_bin), str(fixture)], out)
        if any(not (old / p).is_file() or sha((old / p).read_bytes()) != digest for p, digest in original.items()):
            raise RuntimeError("original historical production/source changed")
        (out / "historical-source-hashes.json").write_text(json.dumps(original, indent=2) + "\n")
        meta = json.loads((fixture / "generation.json").read_text())
        with sqlite3.connect(db) as sql:
            assert sql.execute("PRAGMA user_version").fetchone()[0] == 3
        run("build-current", ["go", "build", "-o", str(new_bin), "./cmd/clawbridge"], ROOT)
        binaries = {"old_generator": sha(old_bin.read_bytes()), "current_cli": sha(new_bin.read_bytes())}
        home = out / "isolated-home"
        ledger = home / "state/linear/ledger"
        ledger.parent.mkdir(mode=0o700, parents=True)
        shutil.copytree(fixture / "ledger", ledger)
        before = files(ledger)
        runtime = {"HOME": str(home), "PATH": str(home), "CLAWBRIDGE_HOME": str(home / "state"), "XDG_CONFIG_HOME": str(home), "XDG_DATA_HOME": str(home), "XDG_CACHE_HOME": str(home)}
        scope, canonical = meta["scope"], meta["canonical_scope"]
        common = [str(new_bin), "--mode", "local", "--db", str(db), "--workspace", scope["workspace"], "--project", scope["project"], "--session", scope["session"], "--instance", scope["args"]["instance"]]

        def invoke(label, input_, extra=(), canonical_scope=False):
            argv = list(common)
            if canonical_scope:
                argv[argv.index("--workspace") + 1] = canonical["workspace_id"]
                argv[argv.index("--project") + 1] = canonical["project_id"]
                argv[argv.index("--session") + 1] = canonical["session_id"]
            argv += ["linear", "create", "--profile", meta["profile_id"], "--operation-id", input_["OperationID"], "--title", input_["Title"], *extra]
            return run(label, argv, out, runtime, okay=False)

        for label, canonical_scope in [("accepted", False), ("accepted-canonical", True)]:
            p = invoke(label, meta["accepted_input"], canonical_scope=canonical_scope)
            assert p.returncode == 0, label + " rejected historical receipt"
            result = json.loads(p.stdout)
            assert result == {"source": "linear", "data": meta["accepted_receipt"]}
        p = invoke("pending", meta["pending_input"])
        assert p.returncode and b"uncertain" in p.stderr.lower()
        p = invoke("changed-v2-intent", meta["accepted_input"], ["--label", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"])
        assert p.returncode and b"different scoped payload" in p.stderr.lower()
        assert files(ledger) == before, "actual CLI changed historical records or left locks/staging"
        assert list((home / "state/linear").iterdir()) == [ledger], "CLI created personal profile/storage outside ledger"
        with sqlite3.connect(db) as sql:
            assert sql.execute("PRAGMA user_version").fetchone()[0] == 4
        result = {"status": "PASS_ACTUAL_OLD_SERIALIZER_CURRENT_CLI_ACCEPTED_PENDING_AND_NEW_INTENT_CONFLICT", "old_source": SOURCE, "old_source_archive_sha256": sha(archive), "original_historical_files_unchanged": len(original), "runtime_generator_sha256": sha(generator.encode()), "binaries": binaries, "original_ledger_files_preserved": before, "schema_before": 3, "schema_after": 4, "accepted_alias_and_canonical_receipts_identical": True, "pending_uncertain_no_replay": True, "new_intent_conflicts": True, "profile_index_absent": True, "credential_operations": "No profile index exists; accepted/pending/conflict path returns before manager Status/HTTP. No credential runtime invoked by harness.", "network": "Builds cached/offline; runtime no profile/transport configuration; no external operation.", "records": records}
        (out / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps({"status": result["status"], "evidence": str(out)}))
    finally:
        shutil.rmtree(scratch)
        (out / "build-scratch-cleaned.json").write_text(json.dumps({"owned_build_directory_absent": not scratch.exists()}) + "\n")


if __name__ == "__main__":
    main()
