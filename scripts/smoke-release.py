#!/usr/bin/env python3
"""Extract a release and exercise its binaries on the matching host platform."""
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tarfile
import tempfile
import zipfile


def run(*args):
    result = subprocess.run(args, check=True, capture_output=True, text=True)
    print(result.stdout, end="")
    if result.stderr:
        print(result.stderr, end="", file=sys.stderr)
    return result.stdout


def main():
    if len(sys.argv) != 4:
        raise SystemExit("Usage: python3 scripts/smoke-release.py ARCHIVE VERSION COMMIT")
    archive = Path(sys.argv[1]).resolve()
    version, commit = sys.argv[2:]
    suffix = ".zip" if archive.name.endswith(".zip") else ".tar.gz"
    name = archive.name.removesuffix(suffix)
    target_os, target_arch = name.rsplit("_", 2)[-2:]
    host_os = {"Linux": "linux", "Darwin": "darwin", "Windows": "windows"}[platform.system()]
    host_arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    if (target_os, target_arch) != (host_os, host_arch):
        raise SystemExit(f"Native smoke requires {target_os}/{target_arch}, host is {host_os}/{host_arch}")
    checksum = archive.with_name(name + ".sha256").read_text().strip()
    expected, filename = checksum.split(maxsplit=1)
    if filename != archive.name:
        raise SystemExit(f"Checksum names {filename}, expected {archive.name}")
    with archive.open("rb") as stream:
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
    if actual != expected:
        raise SystemExit(f"SHA-256 mismatch for {archive}")
    print(f"SHA-256 OK: {archive.name}")
    with tempfile.TemporaryDirectory(prefix="touko-release-smoke-") as temp:
        root = Path(temp)
        if suffix == ".zip":
            with zipfile.ZipFile(archive) as bundle:
                bundle.extractall(root)
        else:
            with tarfile.open(archive) as bundle:
                bundle.extractall(root, filter="data")
        bundle = root / name
        exe = ".exe" if target_os == "windows" else ""
        cli = bundle / "bin" / ("touko" + exe)
        desktop = (bundle / "TOUKO.app/Contents/MacOS/touko-desktop" if target_os == "darwin"
                   else bundle / "bin" / ("touko-desktop" + exe))
        metadata = json.loads(run(str(cli), "version", "--json"))
        if metadata != {"version": version, "commit": commit}:
            raise SystemExit(f"Wrong CLI build metadata: {metadata}")
        desktop_version = run(str(desktop), "--version")
        if version not in desktop_version or commit not in desktop_version:
            raise SystemExit(f"Wrong desktop build metadata: {desktop_version}")
        state = str(root / "state")
        repository = str(root / "gallery.git")
        run(str(cli), "--state-dir", state, "repo", "init", "--local", repository)
        run(str(cli), "--state-dir", state, "verify")
        # Exercise payload IO, SQLite and Git publication, not just version paths.
        image = bundle / "share/doc/touko/docs/images/webview-gallery.png"
        run(str(cli), "--state-dir", state, "add", str(image), "--tag", "release-smoke")
        env = os.environ.copy()
        env.update(GIT_AUTHOR_NAME="TOUKO release smoke", GIT_AUTHOR_EMAIL="smoke@example.invalid",
                   GIT_COMMITTER_NAME="TOUKO release smoke", GIT_COMMITTER_EMAIL="smoke@example.invalid")
        result = subprocess.run([str(cli), "--state-dir", state, "commit", "-m", "release smoke"],
                                env=env, check=True, capture_output=True, text=True)
        print(result.stdout, end="")
        run(str(cli), "--state-dir", state, "verify")
        found = json.loads(run(str(cli), "--state-dir", state, "find", "release-smoke", "--json"))["assets"]
        if len(found) != 1 or found[0]["state"] != "committed":
            raise SystemExit(f"Published image is not visible as committed: {found}")
        print(f"Native release smoke passed: {target_os}/{target_arch}")


if __name__ == "__main__":
    main()
