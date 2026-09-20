#!/usr/bin/env python3
"""Export reviewed product files from a commit, without Git history or research.

This creates a local directory only. It cannot publish or change visibility.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile

FORBIDDEN = {".git", "temp", "research", "datasets", "evidence", "transcripts", "results", "work", "node_modules"}
SECRET = re.compile(rb"(?<![A-Za-z0-9_-])(?:sk-(?:proj-|ant-)[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{32,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[A-Za-z0-9_-]{30,}|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----)")


def export(repo, revision, destination):
    def git(*args):
        return subprocess.check_output(["git", "-C", str(repo), *args], stderr=subprocess.DEVNULL)
    commit = git("rev-parse", "--verify", revision + "^{commit}").decode().strip()
    raw = git("show", commit + ":public-files.json")
    names = json.loads(raw)
    if not isinstance(names, list) or not names or any(not isinstance(n, str) for n in names) or len(set(names)) != len(names):
        raise ValueError("invalid allowlist")
    files = {}
    modes = {}
    for entry in git("ls-tree", "-rz", commit).split(b"\0"):
        if not entry: continue
        meta, name = entry.split(b"\t", 1)
        modes[name.decode()] = meta.split()[0].decode()
    for name in names:
        path = PurePosixPath(name)
        if name == "EXPORT.json" or path.is_absolute() or str(path) != name or ".." in path.parts or set(path.parts) & FORBIDDEN:
            raise ValueError("forbidden export path")
        if path.name.startswith(".env") or path.suffix in (".key", ".pem", ".p12", ".pfx"):
            raise ValueError("credential path")
        if modes.get(name) not in ("100644", "100755"):
            raise ValueError("missing, linked, or nonregular source")
        content = git("show", commit + ":" + name)
        if SECRET.search(content):
            raise ValueError("possible secret; content withheld")
        files[name] = content
    destination = Path(destination).absolute()
    if os.path.lexists(destination):
        raise ValueError("destination exists")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".public-export-", dir=destination.parent) as folder:
        root = Path(folder) / "product"
        root.mkdir()
        for name, content in files.items():
            target = root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
            target.chmod(0o755 if modes[name] == "100755" else 0o644)
        manifest = {"source_commit": commit, "history_included": False,
                    "files": {name: hashlib.sha256(content).hexdigest() for name, content in files.items()},
                    "warning": "Allowlist and heuristic secret scan are not a privacy guarantee. Review contents before publication."}
        (root / "EXPORT.json").write_text(json.dumps(manifest, indent=2) + "\n")
        root.rename(destination)
    return manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination")
    parser.add_argument("--commit", default="HEAD")
    args = parser.parse_args()
    try:
        result = export(Path(__file__).resolve().parent.parent, args.commit, args.destination)
        print(f"Exported {len(result['files'])} allowlisted files; no Git history. Nothing published.")
    except (OSError, ValueError, TypeError, subprocess.SubprocessError):
        print("Export refused. Check commit, allowlist, destination, and secret scan; contents withheld.", file=sys.stderr)
        sys.exit(1)
