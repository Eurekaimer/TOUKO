#!/usr/bin/env python3
"""Measure one-image uploads against an isolated GitHub branch and R2 prefix.

The harness runs the four required cells (old/new binary x R2 on/off), three
repetitions each, over the same 200 images. Every image is staged and committed
on its own. The timed operations run through ``scripts/benchmark-upload-go``,
which calls the production ``internal/cli`` entry point in-process and measures
the add and commit phases with Go's monotonic ``time.Since`` clock. The runner is
compiled from a temporary instrumented copy of each source tree (see
``benchmark-upload-go/instrument.py``): the copy adds only phase-recording spans,
so the measured pipeline is the production one and the source checkout stays
untouched. The Python side prepares state, invokes the runner and aggregates the
per-image phase records into per-cell and four-group CSV/JSON/Markdown summaries.
A phase that did not run is reported as unavailable (null), never as zero.

A measured operation is never retried. On the first add or commit failure the
cell stops, keeps the partial raw data and is marked incomplete; failed attempts
are never folded into the statistics.

Isolation and safety:

* a fresh state directory per cell under a new temporary root;
* a unique GitHub branch per cell (created from the repository's base branch);
* a private GitHub repository, and one of two R2 modes:
  * private mode uses a user-supplied dedicated private R2 bucket (there is no
    production-bucket default; the corpus may contain private screenshots);
  * published mode uses the workspace production bucket only with
    ``--published-mapping``, after every input file matches a migrated mapping
    row by SHA-256 and size and every unique content is re-fetched from its
    public R2 URL and hashed outside timing;
* a unique namespace per cell, which is also the R2 object prefix
  ``objects/<namespace>/``. Published mode always generates a 128-bit random
  prefix; custom prefixes are rejected there;
* in private mode, a non-routable ``*.invalid`` public base URL. That value is
  benchmark metadata only and does not make a public bucket private;
* an exact per-run R2 ownership inventory. This tool never deletes objects and
  never lists a bucket for prefix deletion. R2 conditional DELETE cannot
  guarantee atomicity; remove exact listed paths (or the whole temporary
  private bucket) outside this harness only after confirming ownership.

The optional ``--hash-only`` mode reproduces the separate in-memory SHA-256
microbenchmark with Go timing. It is not upload-stage timing and is never mixed
into the upload statistics.
"""

import argparse
import csv
import hashlib
import hmac
import json
import os
import re
import secrets
import shutil
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from urllib.parse import urlparse

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "benchmark-upload-go"))
import instrument  # noqa: E402  (local benchmark module next to this script)
sys.path.pop(0)

# Exclusive buckets: their durations do not overlap each other. ``other`` is
# the residual CLI work (flag parsing, state/config load, input open, JSON
# output) and closes the sum to total. Nested phases may overlap their parent.
PHASES = [
    "ingest",
    "metadata",
    "install_rest",
    "verify",
    "save",
    "git_prepare",
    "upload",
    "commit_git",
    "r2",
    "plan",
    "journal",
    "finalize",
    "cleanup",
    "other",
    "decode",
    "webp",
    "hash",
    "verify_hash",
    "upload_hash",
    "total_hash",
    "r2_put",
    "r2_ingest",
    "r2_hash",
]
EXCLUSIVE_PHASES = [
    "ingest",
    "metadata",
    "install_rest",
    "verify",
    "save",
    "git_prepare",
    "upload",
    "commit_git",
    "r2",
    "plan",
    "journal",
    "finalize",
    "cleanup",
]
NESTED_PHASES = {
    "decode": "ingest",
    "webp": "ingest",
    "hash": "ingest",
    "verify_hash": "verify",
    "upload_hash": "upload",
    "total_hash": "multiple hash parents",
    "r2_put": "r2",
    "r2_ingest": "r2",
    "r2_hash": "r2",
}

# What each recorded span actually covers. This is written into the JSON so a
# reader never has to infer a phase from its name.
PHASE_DEFINITIONS = {
    "ingest": "whole image.ingest call in add: read source once into a temporary, fsync, header decode, optional WebP",
    "decode": "nested in ingest: stdimage DecodeConfig header parse; with --webp also the full pixel decode inside conversion",
    "webp": "nested in ingest: full pixel decode, WebP encode and fsync; absent unless --webp converts this PNG/JPEG",
    "hash": "nested in ingest: SHA-256 digest writes during the initial source copy",
    "verify_hash": "nested in verify: SHA-256 digest writes during add staging read-back and commit reused-object verification",
    "upload_hash": "nested in upload: SHA-256 digest writes while re-reading staged image bytes for the Git blob upload",
    "total_hash": "sum of add ingest.hash + add verify.hash + commit verify.hash + commit upload.hash + commit ingest.hash; nested and non-additive; SHA-256 writes over image bytes only (no sha256.New/Sum, metadata hashes or SDK signing)",
    "metadata": "PrepareInput plus applyPrepared: asset identity, title/tags/collections, path and local staged plan",
    "install_rest": "installPrepared minus verify and save: staging directory setup and payload rename",
    "verify": "staging verifyFile (size, header, full SHA-256 read-back, fsync) plus reused-object verification in commit",
    "save": "local index save after staging the add",
    "git_prepare": "authoritative ReadSnapshot plus PlanCommit in commit",
    "upload": "image blob uploads plus metadata blob uploads to the Git backend",
    "commit_git": "CreateTree plus CreateCommit plus UpdateRef",
    "plan": "plannedSnapshot construction from the accepted writes",
    "journal": "durable publication journal save",
    "finalize": "local index save after publication",
    "cleanup": "local unreferenced payload cleanup after publication",
    "r2": "whole Cloudflare publication hook: HEAD checks, PUT, read-back verification and ledger save; with R2 off no call is made, the phase is null and the hook's residual overhead lands in other",
    "r2_put": "nested in r2: one PutObject including its read-back HEAD",
    "r2_ingest": "nested in r2: re-reading the committed blob into a temporary before PUT",
    "r2_hash": "nested in r2: SHA-256 writes during that re-read",
    "other": "residual CLI work not covered by an exclusive bucket (flag parsing, config/state load, locking, JSON output)",
}

FIELDS = [
    "index",
    "file",
    "size_bytes",
    "add_ns",
    "add_ms",
    "commit_ns",
    "commit_ms",
    "total_ns",
    "total_ms",
]
for _phase in PHASES:
    FIELDS.extend([_phase + "_ns", _phase + "_ms"])
FIELDS.extend(
    [
        "phases_available",
        "r2_applicable",
        "unavailable_phases",
        "add_ok",
        "commit_ok",
        "ok",
        "error",
        "r2_enabled",
        "r2_status",
        "r2_failed",
        "r2_pending",
        "r2_error_code",
        "r2_object_paths",
    ]
)

# Private mode requires a dedicated bucket. Public mode is only reachable with
# --published-mapping after every input byte has been fetched and hashed from an
# already-public URL; the default workspace bucket/endpoint are used there.
R2_DEFAULT_PUBLIC_BASE = "https://private.invalid"
# A `.invalid` URL is benchmark metadata only. It does not hide objects in a
# public bucket, so it is never accepted as proof of privacy.
PUBLISHED_FETCH_ATTEMPTS = 3
PUBLISHED_PREFIX_PREFIX = "b"
PUBLISHED_PREFIX_BYTES = 16  # 128-bit unpredictable nonce

LABELS = ("old", "new")
R2_STATES = ("off", "on")


def die(message):
    raise SystemExit(message)


def configured_workspace():
    home = os.environ.get("TOUKO_HOME")
    if not home:
        base = os.environ.get("XDG_DATA_HOME") or os.path.expanduser("~/.local/share")
        home = os.path.join(base, "touko")
    path = os.path.join(os.path.expanduser(home), "config.json")
    try:
        with open(path, encoding="utf-8") as handle:
            config = json.load(handle)
    except FileNotFoundError:
        return {}
    except (OSError, ValueError) as error:
        die(f"cannot inspect default TOUKO workspace config: {error}")
    if not isinstance(config, dict):
        die("default TOUKO workspace config is not an object")
    return config


def utc_stamp():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def key_values(values, what):
    result = {}
    for value in values:
        if "=" not in value:
            die(f"{what} must be LABEL=VALUE, got {value!r}")
        label, path = value.split("=", 1)
        label = label.strip().lower()
        if not label or not path:
            die(f"{what} must be LABEL=VALUE, got {value!r}")
        if label in result:
            die(f"duplicate {what} label {label!r}")
        result[label] = os.path.abspath(os.path.expanduser(path))
    return result


def file_sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuse redirects so a public proof never follows away from its origin."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        die(
            f"published content URL {req.full_url} redirected (HTTP {code}) to {newurl}; "
            "redirects are refused"
        )


def public_base_origin(base_url):
    """Parse and validate the configured HTTPS public base."""
    if not isinstance(base_url, str) or not base_url.strip():
        die("the default workspace Cloudflare public_base_url is missing")
    try:
        parsed = urlparse(base_url)
        port = parsed.port
    except ValueError as error:
        die(f"invalid Cloudflare public_base_url {base_url!r}: {error}")
    if parsed.scheme != "https":
        die(f"Cloudflare public_base_url must be HTTPS, got {base_url!r}")
    if not parsed.hostname:
        die(f"Cloudflare public_base_url has no host: {base_url!r}")
    if parsed.username or parsed.password:
        die("Cloudflare public_base_url must not contain credentials")
    if parsed.query or parsed.fragment or parsed.params:
        die("Cloudflare public_base_url must not contain a query, fragment or parameters")
    if parsed.path and not parsed.path.startswith("/"):
        die(f"Cloudflare public_base_url has an invalid path: {base_url!r}")
    if "//" in parsed.path:
        die("Cloudflare public_base_url must not contain an empty path segment")
    if port is not None and not (1 <= port <= 65535):
        die("Cloudflare public_base_url has an invalid port")
    return parsed


def url_under_public_base(url, base):
    """Return True only for an exact HTTPS origin/base-path match.

    The comparison is deliberately strict: same scheme, host, effective port
    and an object path below the configured base path. Userinfo, queries,
    fragments, empty segments and dot segments are refused so a mapping cannot
    point the proof at a different origin or a path-traversal URL.
    """
    if not isinstance(url, str) or not url:
        return False
    try:
        parsed = urlparse(url)
        port = parsed.port
    except ValueError:
        return False
    if parsed.scheme != "https" or not parsed.hostname:
        return False
    if parsed.username or parsed.password:
        return False
    if parsed.query or parsed.fragment or parsed.params:
        return False
    if parsed.hostname.rstrip(".").lower() != base.hostname.rstrip(".").lower():
        return False
    if (port or 443) != (base.port or 443):
        return False
    base_path = base.path.rstrip("/")
    prefix = base_path + "/" if base_path else "/"
    if not parsed.path.startswith(prefix):
        return False
    remainder = parsed.path[len(prefix) :]
    if not remainder or remainder.endswith("/"):
        return False
    segments = remainder.split("/")
    if any(segment in ("", ".", "..") for segment in segments):
        return False
    return True


def load_published_mapping(path):
    """Load only fully migrated rows with a SHA-256, size and new R2 URL."""
    try:
        with open(path, encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError) as error:
        die(f"cannot read published mapping {path}: {error}")
    if isinstance(data, dict):
        data = data.get("items")
    if not isinstance(data, list):
        die(f"published mapping {path} must be a JSON array of objects")
    entries = []
    for index, item in enumerate(data):
        if not isinstance(item, dict):
            die(f"published mapping entry {index} is not an object")
        status = (item.get("status") or "").strip()
        if status != "migrated":
            continue
        sha256 = item.get("sha256")
        size = item.get("size_bytes")
        url = item.get("new_r2_url")
        new_path = item.get("new_path")
        if not isinstance(sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", sha256):
            die(f"published mapping entry {index} has no lowercase SHA-256")
        if isinstance(size, bool) or not isinstance(size, int) or size <= 0:
            die(f"published mapping entry {index} has no positive size_bytes")
        if not isinstance(url, str) or not url:
            die(f"published mapping entry {index} has no new_r2_url")
        if not isinstance(new_path, str) or not new_path:
            die(f"published mapping entry {index} has no new_path")
        if (
            new_path.startswith("/")
            or "\\" in new_path
            or any(part in ("", ".", "..") for part in new_path.split("/"))
        ):
            die(f"published mapping entry {index} has an unsafe new_path: {new_path!r}")
        entries.append(
            {
                "asset_id": item.get("asset_id"),
                "sha256": sha256,
                "size_bytes": size,
                "new_r2_url": url,
                "new_path": new_path,
            }
        )
    if not entries:
        die(f"published mapping {path} has no migrated entries")
    return entries


def fetch_public_content(url, expected_sha256, expected_size, timeout, attempts=PUBLISHED_FETCH_ATTEMPTS):
    """Fetch one already-public object outside timing and verify its bytes.

    No credential is attached: the request carries only a fixed User-Agent.
    Redirects, non-200 responses, oversized/truncated bodies and digest
    mismatches fail closed. Only timeouts and 5xx responses are retried, and
    only during untimed setup.
    """
    try:
        parsed = urlparse(url)
    except ValueError:
        die(f"published content URL is invalid: {url!r}")
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        die(f"published content URL must be HTTP(S): {url!r}")
    if parsed.username or parsed.password:
        die("published content URL must not contain credentials")
    opener = urllib.request.build_opener(_NoRedirect)
    attempts = max(1, attempts)
    for attempt in range(1, attempts + 1):
        try:
            request = urllib.request.Request(
                url,
                headers={
                    "User-Agent": "touko-benchmark-upload/1",
                    "Accept-Encoding": "identity",
                },
            )
            with opener.open(request, timeout=timeout) as response:
                status = getattr(response, "status", 200)
                if status != 200:
                    die(f"published content GET returned HTTP {status} for {url}")
                declared = response.headers.get("Content-Length")
                if declared is not None:
                    try:
                        declared_size = int(declared)
                    except ValueError:
                        die(f"published content GET returned an invalid Content-Length for {url}")
                    if declared_size != expected_size:
                        die(
                            f"published content size mismatch for {url}: "
                            f"Content-Length {declared_size} != {expected_size}"
                        )
                digest = hashlib.sha256()
                total = 0
                while True:
                    chunk = response.read(1 << 20)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > expected_size:
                        die(
                            f"published content size mismatch for {url}: "
                            f"body exceeds {expected_size} bytes"
                        )
                    digest.update(chunk)
        except urllib.error.HTTPError as error:
            if error.code >= 500 and attempt < attempts:
                time.sleep(0.5 * attempt)
                continue
            die(f"published content GET failed for {url}: HTTP {error.code}")
        except (urllib.error.URLError, TimeoutError, socket.timeout) as error:
            if attempt < attempts:
                time.sleep(0.5 * attempt)
                continue
            die(f"published content GET failed for {url}: {error}")
        if total != expected_size:
            die(
                f"published content size mismatch for {url}: "
                f"received {total} bytes, expected {expected_size}"
            )
        actual = digest.hexdigest()
        if not hmac.compare_digest(actual, expected_sha256):
            die(
                f"published content SHA-256 mismatch for {url}: "
                f"received {actual}, expected {expected_sha256}"
            )
        return total
    die(f"published content GET failed for {url}")


def verify_published_corpus(images, entries, base, timeout):
    """Prove every input file is already public before any remote write.

    The mapping is not trusted on its own: each input file must match a
    migrated row by SHA-256 and size, the row URL must be under the exact
    configured HTTPS origin/base path, and one GET per unique content must
    return bytes with the same SHA-256 and size.
    """
    by_content = {}
    for entry in entries:
        if not url_under_public_base(entry["new_r2_url"], base):
            die(
                f"published mapping URL for {entry['new_path']} is outside the configured "
                f"HTTPS public base: {entry['new_r2_url']}"
            )
        by_content.setdefault((entry["sha256"], entry["size_bytes"]), []).append(entry)

    unique = {}
    input_bytes = 0
    for image in images:
        try:
            size = os.path.getsize(image)
            sha256 = file_sha256(image)
        except OSError as error:
            die(f"cannot inspect input image {image}: {error}")
        input_bytes += size
        matches = by_content.get((sha256, size))
        if not matches:
            die(
                f"input image {image} (sha256 {sha256}, {size} bytes) has no migrated "
                "published mapping entry; refusing to use the production bucket"
            )
        unique.setdefault((sha256, size), matches[0]["new_r2_url"])

    contents = []
    unique_bytes = 0
    for (sha256, size), url in sorted(unique.items()):
        fetch_public_content(url, sha256, size, timeout)
        unique_bytes += size
        contents.append({"sha256": sha256, "size_bytes": size, "url": url})
    return {
        "entries": len(entries),
        "input_files": len(images),
        "input_bytes": input_bytes,
        "unique_contents": len(contents),
        "unique_content_bytes": unique_bytes,
        "contents": contents,
    }


def state_r2_config(r2_state, r2):
    """Return the R2 config only for the R2-on cell, never a boolean."""
    return r2 if r2_state == "on" else None


def resolve_r2_settings(
    workspace,
    *,
    bucket,
    endpoint,
    public_base,
    private_confirmed,
    prefix,
    published_mapping,
    credentials,
    images,
    timeout,
    stamp,
):
    """Resolve private mode or the fail-closed published-production mode."""
    configured = workspace.get("cloudflare") or {}
    if not isinstance(configured, dict):
        die("default TOUKO workspace Cloudflare config is not an object")

    if published_mapping:
        if private_confirmed:
            die("--r2-bucket-private cannot be combined with --published-mapping")
        if prefix:
            die("--r2-prefix is not allowed with --published-mapping; the benchmark generates a random prefix")
        if public_base is not None:
            die("--r2-public-base is fixed by the workspace config in --published-mapping mode")
        configured_bucket = configured.get("bucket")
        configured_endpoint = configured.get("endpoint")
        configured_base = configured.get("public_base_url")
        if not isinstance(configured_bucket, str) or not configured_bucket:
            die("--published-mapping requires a default workspace Cloudflare bucket")
        if not isinstance(configured_endpoint, str) or not configured_endpoint:
            die("--published-mapping requires a default workspace Cloudflare endpoint")
        if bucket and bucket != configured_bucket:
            die(
                f"--r2-bucket {bucket!r} does not match the default workspace bucket "
                f"{configured_bucket!r}"
            )
        if endpoint and endpoint.rstrip("/") != configured_endpoint.rstrip("/"):
            die(
                f"--r2-endpoint {endpoint!r} does not match the default workspace endpoint "
                f"{configured_endpoint!r}"
            )
        if re.search(r"[/\\\s]", configured_bucket):
            die("the configured R2 bucket name contains a path separator or whitespace")
        base = public_base_origin(configured_base)
        mapping_path = os.path.abspath(os.path.expanduser(published_mapping))
        entries = load_published_mapping(mapping_path)
        report = verify_published_corpus(images, entries, base, timeout)
        report["mapping"] = mapping_path
        report["mapping_sha256"] = file_sha256(mapping_path)
        report["public_base_url"] = configured_base
        return {
            "mode": "published",
            "endpoint": configured_endpoint,
            "bucket": configured_bucket,
            "public_base_url": configured_base,
            "credentials": credentials,
            "prefix": PUBLISHED_PREFIX_PREFIX
            + secrets.token_hex(PUBLISHED_PREFIX_BYTES),
            "private_bucket_confirmed": False,
            "published": report,
        }

    if not bucket or not endpoint:
        die(
            "the R2-on groups require a dedicated private bucket: pass --r2-endpoint and "
            "--r2-bucket. The production bucket is only allowed with --published-mapping "
            "after its bytes are proven already public."
        )
    if bucket == configured.get("bucket"):
        die(
            "refusing to benchmark against the default workspace's R2 bucket; pass "
            "--published-mapping only when every input file is already public and verified"
        )
    if re.search(r"[/\\\s]", bucket):
        die("--r2-bucket must be a bucket name without path separators or whitespace")
    if not private_confirmed:
        die(
            "pass --r2-bucket-private only after verifying the bucket is dedicated to this run, "
            "private, and has no public access; the corpus contains private screenshots"
        )
    base_url = public_base or R2_DEFAULT_PUBLIC_BASE
    try:
        parsed = urlparse(base_url)
    except ValueError:
        die(f"invalid --r2-public-base: {base_url!r}")
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or not parsed.hostname.endswith(".invalid")
    ):
        die(
            "--r2-public-base must be a non-routable https://*.invalid placeholder. "
            "It is benchmark metadata only and does not make a public bucket private."
        )
    resolved_prefix = (prefix or "").strip().lower() or f"benchmark-{stamp.lower()}"
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", resolved_prefix):
        die("--r2-prefix must be a lowercase slug using [a-z0-9_-]")
    if len(resolved_prefix) > 40:
        die("--r2-prefix must be at most 40 characters")
    return {
        "mode": "private",
        "endpoint": endpoint,
        "bucket": bucket,
        "public_base_url": base_url,
        "credentials": credentials,
        "prefix": resolved_prefix,
        "private_bucket_confirmed": True,
        "published": None,
    }


def git_head(path):
    try:
        proc = subprocess.run(
            ["git", "-C", path, "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if proc.returncode != 0:
        return None
    return proc.stdout.strip() or None


def binary_build_info(binary):
    try:
        proc = subprocess.run(
            ["go", "version", "-m", binary],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return {}
    if proc.returncode != 0:
        return {}
    info = {}
    for line in proc.stdout.splitlines():
        parts = line.strip().split("\t")
        if len(parts) >= 2 and parts[0] == "build" and "=" in parts[1]:
            key, value = parts[1].split("=", 1)
            info[key] = value
    return info


def source_dirty(source):
    try:
        proc = subprocess.run(
            ["git", "-C", source, "status", "--porcelain", "--untracked-files=no"],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if proc.returncode != 0:
        return None
    return bool(proc.stdout.strip())


def go_directive(source):
    try:
        with open(os.path.join(source, "go.mod"), encoding="utf-8") as handle:
            for line in handle:
                match = re.match(r"^go\s+(\S+)\s*$", line.strip())
                if match:
                    return match.group(1)
    except OSError:
        pass
    return "1.27.1"


def command_env():
    env = os.environ.copy()
    env.pop("TOUKO_HOME", None)
    return env


def run_checked(command, env, timeout, label, cwd=None):
    try:
        proc = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=env,
            cwd=cwd,
        )
    except subprocess.TimeoutExpired:
        die(f"{label} timed out after {timeout}s")
    except OSError as error:
        die(f"{label} could not start: {error}")
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        die(f"{label} failed (exit {proc.returncode}): {detail[:2000]}")
    return proc


def gh_api(args, timeout=120, check=True):
    command = ["gh", "api", *args]
    try:
        proc = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    except FileNotFoundError:
        die("gh is required to create isolated benchmark branches")
    except subprocess.TimeoutExpired:
        die("gh api timed out")
    if check and proc.returncode != 0:
        return proc, False
    return proc, proc.returncode == 0


def repo_default_branch(repo, create_repo, timeout):
    proc, ok = gh_api([f"repos/{repo}", "--jq", ".default_branch"], timeout=timeout, check=False)
    if ok and proc.stdout.strip():
        return proc.stdout.strip()
    if not create_repo:
        fail = (proc.stderr or proc.stdout or "").strip()
        die(
            f"cannot read repository {repo} (or it has no commits); "
            f"create it with a README or pass --create-repo: {fail[:500]}"
        )
    create = subprocess.run(
        ["gh", "repo", "create", repo, "--private", "--add-readme"],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if create.returncode != 0:
        die(f"gh repo create {repo} failed: {(create.stderr or create.stdout).strip()[:1000]}")
    for _ in range(10):
        time.sleep(1)
        proc, ok = gh_api([f"repos/{repo}", "--jq", ".default_branch"], timeout=timeout, check=False)
        if ok and proc.stdout.strip():
            return proc.stdout.strip()
    die(f"repository {repo} was created but its default branch is not readable")


def ensure_private_repo(repo, timeout):
    proc, ok = gh_api([f"repos/{repo}", "--jq", ".private"], timeout=timeout, check=False)
    if not ok:
        detail = (proc.stderr or proc.stdout or "").strip()
        die(f"cannot read repository visibility for {repo}: {detail[:500]}")
    if proc.stdout.strip().lower() != "true":
        die(
            f"repository {repo} is not private; the image corpus contains private screenshots "
            "and must not be published"
        )


def ensure_clean_base(repo, base, timeout):
    """Refuse to fork benchmark branches from a gallery that already has assets."""
    proc, ok = gh_api(
        [f"repos/{repo}/contents/.touko/repository.json?ref={base}"],
        timeout=timeout,
        check=False,
    )
    if ok:
        die(
            f"base branch {base!r} already contains .touko/repository.json; "
            "use an empty repository or a clean base branch"
        )
    detail = (proc.stderr or proc.stdout or "").strip()
    if "Not Found" not in detail and "404" not in detail:
        die(f"cannot verify that base branch {base!r} is clean: {detail[:500]}")


def create_branch(repo, base, branch, timeout):
    proc, ok = gh_api(
        [f"repos/{repo}/git/ref/heads/{base}", "--jq", ".object.sha"],
        timeout=timeout,
        check=False,
    )
    if not ok or not proc.stdout.strip():
        detail = (proc.stderr or proc.stdout or "").strip()
        die(f"cannot read base branch {base!r}: {detail[:500]}")
    sha = proc.stdout.strip()
    proc, ok = gh_api(
        [
            "--method",
            "POST",
            f"repos/{repo}/git/refs",
            "-f",
            f"ref=refs/heads/{branch}",
            "-f",
            f"sha={sha}",
        ],
        timeout=timeout,
        check=False,
    )
    if not ok:
        detail = (proc.stderr or proc.stdout or "").strip()
        die(f"cannot create branch {branch!r}: {detail[:1000]}")
    return sha


def analyse_corpus(images):
    """Record identical files without changing the supplied dataset.

    Old builds use SHA-addressed object paths, so an intra-run duplicate can
    reuse one image blob. New builds use asset-UUID paths and publish one blob
    per asset. The difference is real work and must be reported, not removed.
    """
    by_size = {}
    for image in images:
        size = os.path.getsize(image)
        by_size.setdefault(size, []).append(image)
    groups = []
    duplicate_files = 0
    for size, candidates in sorted(by_size.items()):
        if len(candidates) < 2:
            continue
        by_hash = {}
        for image in candidates:
            by_hash.setdefault(file_sha256(image), []).append(os.path.basename(image))
        for digest, names in sorted(by_hash.items()):
            if len(names) < 2:
                continue
            duplicate_files += len(names) - 1
            groups.append(
                {
                    "sha256": digest,
                    "size_bytes": size,
                    "files": sorted(names),
                    "duplicate_files": len(names) - 1,
                }
            )
    total_bytes = sum(os.path.getsize(image) for image in images)
    return {
        "total_bytes": total_bytes,
        "duplicate_groups": groups,
        "duplicate_file_count": duplicate_files,
        "distinct_content_bytes": total_bytes
        - sum(group["size_bytes"] * group["duplicate_files"] for group in groups),
        "old_path_deduplicates_image_blobs": True,
        "new_path_publishes_one_image_blob_per_asset": True,
        "note": (
            "Identical files are kept. Old SHA-addressed paths can reuse one image blob for a "
            "duplicate; new asset-UUID paths publish a separate image blob for every asset. "
            "The old and new groups therefore perform different object work for these files."
        ),
    }


def image_files(directory, expected):
    try:
        names = sorted(
            name
            for name in os.listdir(directory)
            if name.lower().endswith((".png", ".jpg", ".jpeg", ".gif", ".webp"))
            and os.path.isfile(os.path.join(directory, name))
        )
    except OSError as error:
        die(f"cannot read image directory {directory}: {error}")
    if not names:
        die(f"no images found in {directory}")
    if expected and len(names) != expected:
        die(f"expected exactly {expected} images in {directory}, found {len(names)}")
    return [os.path.join(directory, name) for name in names]


def write_json(path, value):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(value, handle, ensure_ascii=False, indent=2)
        handle.write("\n")


def write_config_with_r2(state, r2):
    path = os.path.join(state, "config.json")
    with open(path, encoding="utf-8") as handle:
        config = json.load(handle)
    config["cloudflare"] = {
        "enabled": True,
        "endpoint": r2["endpoint"],
        "bucket": r2["bucket"],
        "public_base_url": r2["public_base_url"],
    }
    temporary = path + ".benchmark"
    with open(temporary, "w", encoding="utf-8") as handle:
        json.dump(config, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    os.chmod(temporary, 0o600)
    os.replace(temporary, path)
    target = os.path.join(state, "cloudflare")
    os.makedirs(target, mode=0o700, exist_ok=True)
    destination = os.path.join(target, "credentials.json")
    shutil.copyfile(r2["credentials"], destination)
    os.chmod(destination, 0o600)


def configure_state(binary, state, repo, branch, r2, env, timeout):
    if os.path.exists(state):
        die(f"state directory already exists: {state}")
    os.makedirs(state, mode=0o700)
    run_checked(
        [binary, "--state-dir", state, "repo", "use", repo, "--branch", branch, "--json"],
        env,
        timeout,
        f"repo use ({repo}@{branch})",
    )
    run_checked(
        [binary, "--state-dir", state, "repo", "init", "--json"],
        env,
        timeout,
        f"repo init ({repo}@{branch})",
    )
    if r2:
        write_config_with_r2(state, r2)
    run_checked(
        [binary, "--state-dir", state, "sync", "--json"],
        env,
        timeout,
        f"sync ({repo}@{branch})",
    )


def build_runner(source, label, root, template, env, timeout, allow_dirty):
    dirty = source_dirty(source)
    if dirty and not allow_dirty:
        die(
            f"source {source} has uncommitted tracked changes; build the timing runner from a "
            "clean checkout of the revision that produced the binary, or pass --allow-dirty-source"
        )
    instrumented = os.path.join(root, "sources", label)
    try:
        instrumentation = instrument.prepare_source(source, instrumented)
    except instrument.InstrumentationError as error:
        die(f"cannot instrument source for {label}: {error}")
    build_dir = os.path.join(root, "runner-build", label)
    os.makedirs(build_dir, exist_ok=True)
    module = "github.com/Eurekaimer/TOUKO/benchupload"
    go_mod = (
        f"module {module}\n\n"
        f"go {go_directive(source)}\n\n"
        "require github.com/Eurekaimer/TOUKO v0.0.0\n\n"
        f"replace github.com/Eurekaimer/TOUKO => {json.dumps(instrumented)}\n"
    )
    with open(os.path.join(build_dir, "go.mod"), "w", encoding="utf-8") as handle:
        handle.write(go_mod)
    source_sum = os.path.join(source, "go.sum")
    if os.path.isfile(source_sum):
        shutil.copyfile(source_sum, os.path.join(build_dir, "go.sum"))
    shutil.copyfile(template, os.path.join(build_dir, "main.go"))
    output = os.path.join(root, "runners", label)
    os.makedirs(os.path.dirname(output), exist_ok=True)
    build_env = env.copy()
    build_env.update({"GOPROXY": "off", "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=mod"})
    run_checked(
        ["go", "build", "-trimpath", "-buildvcs=false", "-o", output, "."],
        build_env,
        timeout,
        f"build benchmark runner for {label}",
        cwd=build_dir,
    )
    return output, dirty, instrumentation


def _ns(value):
    return None if value is None else int(value)


def _sum_present(values):
    present = [value for value in values if value is not None]
    return sum(present) if present else None


def derive_phases(raw):
    """Map raw span names to the report's phase buckets.

    A missing span means the phase did not run or was not measured; it stays
    None instead of becoming a fabricated zero.
    """
    add = raw.get("phases_add")
    commit = raw.get("phases_commit")
    if not isinstance(add, dict) or not isinstance(commit, dict) or not (add or commit):
        return None
    ingest = _ns(add.get("add.ingest"))
    verify_add = _ns(add.get("add.verify"))
    save = _ns(add.get("add.save"))
    install = _ns(add.get("add.install"))
    install_rest = None
    if install is not None:
        install_rest = max(0, install - (verify_add or 0) - (save or 0))
    hashes = {
        "add_ingest": _ns(add.get("ingest.hash")),
        "add_verify": _ns(add.get("verify.hash")),
        "commit_verify": _ns(commit.get("verify.hash")),
        "commit_upload": _ns(commit.get("upload.hash")),
        "commit_ingest": _ns(commit.get("ingest.hash")),
    }
    phases = {
        "ingest": ingest,
        "metadata": _ns(add.get("add.metadata")),
        "install_rest": install_rest,
        "verify": _sum_present([verify_add, _ns(commit.get("commit.verify"))]),
        "save": save,
        "git_prepare": _ns(commit.get("commit.prepare")),
        "upload": _sum_present(
            [_ns(commit.get("commit.upload")), _ns(commit.get("commit.metadata_blob"))]
        ),
        "commit_git": _sum_present(
            [
                _ns(commit.get("commit.tree")),
                _ns(commit.get("commit.commit")),
                _ns(commit.get("commit.ref")),
            ]
        ),
        "r2": _ns(commit.get("commit.r2")),
        "plan": _ns(commit.get("commit.plan")),
        "journal": _ns(commit.get("commit.journal")),
        "finalize": _ns(commit.get("commit.finalize")),
        "cleanup": _ns(commit.get("commit.cleanup")),
        "decode": _ns(add.get("ingest.decode")),
        "webp": _ns(add.get("ingest.webp")),
        "hash": hashes["add_ingest"],
        "verify_hash": _sum_present([hashes["add_verify"], hashes["commit_verify"]]),
        "upload_hash": hashes["commit_upload"],
        "total_hash": _sum_present(list(hashes.values())),
        "r2_put": _ns(commit.get("commit.r2_put")),
        "r2_ingest": _ns(commit.get("ingest.stream")),
        "r2_hash": hashes["commit_ingest"],
    }
    total = _sum_present([_ns(raw.get("add_ns")), _ns(raw.get("commit_ns"))])
    exclusive = [phases[name] for name in EXCLUSIVE_PHASES if phases[name] is not None]
    phases["other"] = None
    if total is not None:
        phases["other"] = max(0, total - sum(exclusive))
    return phases


def normalise_record(raw, index, image, r2_state, webp_convert):
    record = {
        "index": index,
        "file": os.path.basename(image),
        "size_bytes": os.path.getsize(image),
        "add_ns": None,
        "commit_ns": None,
        "total_ns": None,
        "add_ok": False,
        "commit_ok": False,
        "ok": False,
        "error": "",
        "phases_available": False,
        "r2_applicable": r2_state == "on",
        "r2_enabled": False,
        "r2_status": "",
        "r2_failed": 0,
        "r2_pending": 0,
        "r2_error_code": "",
    }
    if isinstance(raw, dict):
        for name in (
            "add_ns",
            "commit_ns",
            "total_ns",
            "add_ok",
            "commit_ok",
            "ok",
            "error",
            "r2_items",
            "r2_enabled",
            "r2_status",
            "r2_failed",
            "r2_pending",
            "r2_error_code",
        ):
            if name in raw:
                record[name] = raw[name]
    for name in ("add_ns", "commit_ns", "total_ns"):
        value = _ns(record[name])
        record[name] = value
        record[name.replace("_ns", "_ms")] = None if value is None else round(value / 1e6, 3)
    phases = derive_phases(raw) if isinstance(raw, dict) else None
    record["phases_available"] = phases is not None
    unavailable = {}
    if phases is None:
        phases = {name: None for name in PHASES}
        unavailable = {
            name: "phase instrumentation was not present in the runner output"
            for name in PHASES
        }
    else:
        if r2_state != "on":
            # The publication hook still returns immediately, but no R2 call
            # was made; report the phase as not applicable, never as zero.
            for name in ("r2", "r2_put", "r2_ingest", "r2_hash"):
                phases[name] = None
                unavailable[name] = "R2 disabled for this cell"
            exclusive = [phases[name] for name in EXCLUSIVE_PHASES if phases[name] is not None]
            total = record["total_ns"]
            phases["other"] = None if total is None else max(0, total - sum(exclusive))
        if phases.get("webp") is None:
            extension = os.path.splitext(image)[1].lower()
            if not webp_convert:
                unavailable["webp"] = "WebP conversion disabled for this run"
            elif extension in (".gif", ".webp"):
                unavailable["webp"] = "input format is never converted"
            else:
                unavailable["webp"] = "conversion span was not recorded"
        for name in PHASES:
            if phases.get(name) is None and name not in unavailable:
                unavailable[name] = "span was not recorded for this image"
    for name in PHASES:
        value = phases.get(name)
        record[name + "_ns"] = value
        record[name + "_ms"] = None if value is None else round(value / 1e6, 3)
    record["phase_unavailable"] = unavailable
    record["unavailable_phases"] = "; ".join(
        f"{name}={reason}" for name, reason in unavailable.items()
    )
    failed = record.get("r2_failed")
    pending = record.get("r2_pending")
    invalid_counts = (
        isinstance(failed, bool)
        or not isinstance(failed, int)
        or isinstance(pending, bool)
        or not isinstance(pending, int)
    )
    if r2_state == "on":
        if invalid_counts:
            failed, pending = 0, 0
        record["r2_failed"] = failed
        record["r2_pending"] = pending
        if record.get("ok"):
            if record.get("r2_enabled") is not True:
                record["ok"] = False
                record["error"] = "R2-on record was not marked r2_enabled by the runner"
            elif record.get("r2_status") != "synced":
                record["ok"] = False
                record["error"] = (
                    f"R2 publication not synced (status={record.get('r2_status')!r}, "
                    f"failed={failed}, pending={pending}, "
                    f"error_code={record.get('r2_error_code')!r})"
                )
            elif failed != 0 or pending != 0 or invalid_counts:
                record["ok"] = False
                record["error"] = (
                    f"R2 publication incomplete (status={record.get('r2_status')!r}, "
                    f"failed={record.get('r2_failed')}, pending={record.get('r2_pending')})"
                )
    else:
        record["r2_enabled"] = False
        record["r2_failed"] = 0
        record["r2_pending"] = 0
        record["r2_status"] = ""
        record["r2_error_code"] = ""
        record["r2_items"] = []
    record["ok"] = bool(record.get("ok"))
    record["r2_object_paths"] = ";".join(
        item.get("object_path", "")
        for item in record.get("r2_items") or []
        if item.get("object_path")
    )
    return record


def measure_cell(runner, state, images, namespace, group, rep, r2_state, raw_path, env, timeout, webp):
    records = []
    aborted_at = None
    started = time.monotonic()
    with open(raw_path, "w", encoding="utf-8") as raw:
        for index, image in enumerate(images):
            command = [
                runner,
                "-state",
                state,
                "-image",
                image,
                "-namespace",
                namespace,
                "-index",
                str(index),
                "-message",
                f"benchmark: {group} r2={namespace} rep={rep} image={index}",
                f"-r2-enabled={'true' if r2_state == 'on' else 'false'}",
            ]
            if webp:
                command.append("-webp")
            error = ""
            raw_record = None
            try:
                proc = subprocess.run(
                    command,
                    capture_output=True,
                    text=True,
                    timeout=timeout,
                    env=env,
                )
                output = proc.stdout.strip()
                if output:
                    raw_record = json.loads(output.splitlines()[-1])
                if proc.returncode != 0 and not error:
                    error = (proc.stderr or proc.stdout or "").strip()
            except subprocess.TimeoutExpired:
                error = f"runner timed out after {timeout}s"
            except (json.JSONDecodeError, OSError) as failure:
                error = f"runner result unreadable: {failure}"
            record = normalise_record(raw_record, index, image, r2_state, webp)
            if error and not record.get("error"):
                record["error"] = error[:1000]
            if not record["ok"] and not record["error"]:
                record["error"] = "stage or publish failed"
            raw.write(json.dumps(record, ensure_ascii=False) + "\n")
            raw.flush()
            records.append(record)
            print(
                f"  {index + 1:3d}/{len(images)} {record['file']} "
                f"add={fmt(record['add_ms'], 1)}ms commit={fmt(record['commit_ms'], 1)}ms"
                f" ingest={fmt(record['ingest_ms'], 1)}ms"
                f" git={fmt(record['git_prepare_ms'], 1)}ms"
                f" upload={fmt(record['upload_ms'], 1)}ms"
                f" r2={fmt(record['r2_ms'], 1)}ms"
                + ("" if record["ok"] else f" FAILED {record['error'][:160]}"),
                flush=True,
            )
            if not record["ok"]:
                aborted_at = index
                break
    return records, aborted_at, time.monotonic() - started


def fmt(value, digits=3):
    return "n/a" if value is None else f"{value:.{digits}f}"


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    position = max(0, min(len(ordered) - 1, int(round(fraction * (len(ordered) - 1)))))
    return ordered[position]


def metric_stats(values):
    if not values:
        return {"count": 0, "mean": None, "median": None, "p95": None, "min": None, "max": None}
    return {
        "count": len(values),
        "mean": round(statistics.fmean(values), 3),
        "median": round(statistics.median(values), 3),
        "p95": round(percentile(values, 0.95), 3),
        "min": round(min(values), 3),
        "max": round(max(values), 3),
    }


def phase_metrics(records):
    metrics = {}
    for phase in PHASES:
        metrics[phase + "_ms"] = metric_stats(
            [record[phase + "_ms"] for record in records if record.get(phase + "_ms") is not None]
        )
    for name in ("add", "commit", "total"):
        metrics[name + "_ms"] = metric_stats(
            [record[name + "_ms"] for record in records if record.get(name + "_ms") is not None]
        )
    return metrics


def summarise(records, expected, aborted_at):
    ok = [record for record in records if record["ok"]]
    total_ms = [record["total_ms"] for record in ok if record["total_ms"] is not None]
    bytes_total = sum(record["size_bytes"] for record in ok)
    complete = len(ok) == expected and len(records) == expected and aborted_at is None
    phases_measured = sum(1 for record in ok if record["phases_available"])
    return {
        "expected_images": expected,
        "attempted": len(records),
        "succeeded": len(ok),
        "failed": len(records) - len(ok),
        "complete": complete,
        "aborted_at": aborted_at,
        "total_bytes": bytes_total,
        "phases_available": bool(ok) and phases_measured == len(ok),
        "phases_measured_images": phases_measured,
        "metrics": phase_metrics(ok),
        "throughput_mb_s": round(bytes_total / 1e6 / (sum(total_ms) / 1000.0), 6)
        if total_ms and sum(total_ms) > 0
        else None,
    }


def write_cell_outputs(base, metadata, summary, records):
    write_json(base + ".json", {"metadata": metadata, "summary": summary, "records": records})
    with open(base + ".csv", "w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=FIELDS)
        writer.writeheader()
        for record in records:
            writer.writerow({field: record.get(field, "") for field in FIELDS})

    lines = [f"# Upload benchmark: {metadata['group']} / R2 {metadata['r2']} / rep {metadata['rep']}", ""]
    lines.append(f"- generated: {metadata['generated_at']}")
    lines.append(f"- repo: `{metadata['repo']}`")
    lines.append(f"- branch: `{metadata['branch']}`")
    lines.append(f"- namespace / R2 prefix: `{metadata['namespace']}` (`objects/{metadata['namespace']}/`)")
    lines.append(
        f"- R2 mode: `{metadata['r2'].get('mode', 'private')}`; "
        f"bucket: `{metadata['r2'].get('bucket')}`; public base: `{metadata['r2'].get('public_base_url')}`"
    )
    lines.append(f"- binary: `{metadata['binary']}` (sha256 `{metadata['binary_sha256']}`)")
    if metadata.get("source"):
        lines.append(f"- source: `{metadata['source']}` at `{metadata.get('source_head') or 'unknown'}`")
    lines.append(f"- runner: `{metadata['runner']}` (sha256 `{metadata['runner_sha256']}`)")
    lines.append(f"- images: {metadata['image_count']} ({metadata['total_bytes']} bytes)")
    corpus = metadata.get("corpus", {})
    if corpus.get("duplicate_groups"):
        pairs = "; ".join(
            f"{', '.join(group['files'])} (sha256 {group['sha256'][:12]}…)"
            for group in corpus["duplicate_groups"]
        )
        lines.append(f"- identical files kept in the corpus: {pairs}")
        lines.append(
            "- corpus caveat: old SHA-addressed paths can reuse one image blob for identical bytes; "
            "new UUID paths publish one image blob per asset. This cell's object work reflects its layout."
        )
    lines.append(
        "- timing: Go `time.Since` spans inside the staged add and published commit; setup and "
        "process start are outside the timed calls; measured operations are never retried"
    )
    lines.append(
        "- measured code: the runner compiled from an instrumented copy of the listed source; "
        "the supplied binary is used only for untimed setup"
    )
    lines.append(
        "- phases: exclusive buckets are ingest, metadata, install_rest, verify, save, git_prepare, "
        "upload, commit_git, r2, plan, journal, finalize, cleanup and other (CLI residual); "
        "decode, webp and hash are nested in ingest; verify_hash is nested in verify; "
        "upload_hash is nested in upload; total_hash sums image-byte hash writes and is "
        "nested/non-additive; r2_put/r2_ingest/r2_hash are nested in r2"
    )
    if not summary["phases_available"]:
        lines.append(
            "- **phase instrumentation unavailable**: per-phase values are omitted, not zero"
        )
    reasons = {}
    for record in records:
        for phase, reason in record.get("phase_unavailable", {}).items():
            reasons.setdefault(phase, set()).add(reason)
    if reasons:
        detail = "; ".join(
            f"{phase}: {' / '.join(sorted(values))}" for phase, values in sorted(reasons.items())
        )
        lines.append(f"- unavailable phases: {detail}")
    lines.append("")
    if summary["complete"]:
        lines.append(f"**Complete: {summary['succeeded']}/{summary['expected_images']} images.**")
    else:
        lines.append(
            f"**INCOMPLETE: {summary['succeeded']}/{summary['expected_images']} images succeeded, "
            f"{summary['failed']} failed, stopped at index {summary['aborted_at']}. "
            "Statistics below cover only the successful images and must not be compared with "
            "complete cells.**"
        )
    lines.append("")
    lines.append("| metric | count | mean ms | median ms | p95 ms | min ms | max ms |")
    lines.append("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
    for name in ("add_ms", "commit_ms", "total_ms"):
        stats = summary["metrics"][name]
        lines.append(
            f"| {name} | {stats['count']} | {fmt(stats['mean'])} | {fmt(stats['median'])} | "
            f"{fmt(stats['p95'])} | {fmt(stats['min'])} | {fmt(stats['max'])} |"
        )
    for phase in PHASES:
        stats = summary["metrics"][phase + "_ms"]
        parent = NESTED_PHASES.get(phase)
        label = f"{phase} (nested in {parent})" if parent else phase
        lines.append(
            f"| {label} | {stats['count']} | {fmt(stats['mean'])} | {fmt(stats['median'])} | "
            f"{fmt(stats['p95'])} | {fmt(stats['min'])} | {fmt(stats['max'])} |"
        )
    lines.append("")
    lines.append(f"- successful bytes: {summary['total_bytes']}")
    lines.append(f"- throughput (successful total): {fmt(summary['throughput_mb_s'])} MB/s")
    lines.append(f"- diagnostic wall time: {metadata['wall_seconds']} s")
    lines.append("")
    lines.append("Per-image records are in the adjacent `.json` and `.csv`; the `.jsonl` is the raw stream.")
    lines.append("")
    with open(base + ".md", "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))


def probe_r2(runner, endpoint, bucket, public_base, credentials, probe_key, timeout):
    command = [
        runner,
        "-r2-bucket",
        bucket,
        "-r2-endpoint",
        endpoint,
        "-r2-public-base",
        public_base,
        "-r2-credentials",
        credentials,
        "-r2-probe-key",
        probe_key,
    ]
    proc = run_checked(command, command_env(), timeout, "R2 bucket/credential probe")
    try:
        result = json.loads(proc.stdout)
    except json.JSONDecodeError as error:
        die(f"R2 probe returned unreadable JSON: {error}")
    if not result.get("ok"):
        die(f"R2 probe did not confirm bucket access: {result}")
    return result


def write_r2_inventory(out_dir, stamp, metadata, cells):
    observed = []
    violations = []
    for cell in cells:
        namespace = cell["metadata"].get("namespace")
        prefix = f"objects/{namespace}/"
        for record in cell["records"]:
            for item in record.get("r2_items") or []:
                path = item.get("object_path", "")
                if not path.startswith(prefix):
                    violations.append(
                        {
                            "cell": cell["metadata"]["group"],
                            "path": path,
                            "expected_prefix": prefix,
                        }
                    )
                observed.append(
                    {
                        "cell": cell["metadata"]["group"],
                        "namespace": namespace,
                        "index": record.get("index"),
                        "file": record.get("file"),
                        "object_path": path,
                        "action": item.get("action", ""),
                        "status": item.get("status", ""),
                        "error_code": item.get("error_code", ""),
                    }
                )
    unique_paths = sorted({item["object_path"] for item in observed if item["object_path"]})
    mode = metadata["r2"].get("mode")
    if mode == "published":
        cleanup_note = (
            "This is an evidence inventory, not deletion authorization. This run wrote to the "
            "configured production bucket after every input byte was verified from its public URL. "
            "Never delete the bucket. Remove only the exact object paths listed here after confirming "
            "ownership; do not list or delete by a generic prefix. R2 conditional DELETE cannot "
            "guarantee atomicity, and this harness never deletes objects."
        )
    else:
        cleanup_note = (
            "This is an evidence inventory, not deletion authorization. The bucket is expected to "
            "be temporary and private. Delete the whole bucket through the Cloudflare account API or "
            "dashboard after copying the results; do not delete keys by a generic prefix, and do not "
            "rely on conditional DELETE being enforced. This harness never deletes objects."
        )
    payload = {
        "mode": mode,
        "bucket": metadata["r2"]["bucket"],
        "endpoint": metadata["r2"]["endpoint"],
        "prefix": metadata["r2"]["prefix"],
        "observed_items": observed,
        "unique_object_paths": unique_paths,
        "prefix_violations": violations,
        "cleanup_note": cleanup_note,
    }
    base = unique_base(out_dir, f"upload-benchmark-{stamp}-r2-inventory")
    write_json(base + ".json", payload)
    lines = [
        "# R2 ownership inventory",
        "",
        f"- mode: `{payload['mode']}`",
        f"- bucket: `{payload['bucket']}`",
        f"- endpoint: `{payload['endpoint']}`",
        f"- prefix: `{payload['prefix']}`",
        f"- observed item rows: {len(observed)}",
        f"- unique object paths: {len(unique_paths)}",
        f"- prefix violations: {len(violations)}",
        "",
        payload["cleanup_note"],
        "",
    ]
    if unique_paths:
        lines.append("## Exact object paths observed")
        lines.append("")
        for path in unique_paths:
            lines.append(f"- `{path}`")
        lines.append("")
    with open(base + ".md", "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))
    return base, violations


def write_setup_failure(base, metadata, summary):
    write_json(base + ".json", {"metadata": metadata, "summary": summary, "records": []})
    lines = [
        f"# Upload benchmark: {metadata['group']} / R2 {metadata['r2']} / rep {metadata['rep']}",
        "",
        f"- generated: {metadata['generated_at']}",
        f"- repo: `{metadata['repo']}`",
        f"- branch: `{metadata['branch']}`",
        f"- namespace / R2 prefix: `{metadata['namespace']}`",
        f"- binary: `{metadata['binary']}`",
        "",
        "**SETUP FAILED; no timed image was measured. This cell must not be compared.**",
        "",
        f"- error: `{summary['setup_error']}`",
        "",
    ]
    with open(base + ".md", "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))


def empty_metrics():
    return {
        name + "_ms": metric_stats([])
        for name in (*PHASES, "add", "commit", "total")
    }


def phase_shares(records):
    shares = {}
    for phase in PHASES:
        ratios = [
            100.0 * record[phase + "_ms"] / record["total_ms"]
            for record in records
            if record.get(phase + "_ms") is not None
            and record.get("total_ms")
        ]
        shares[phase] = metric_stats(ratios)
    return shares


def aggregate_cells(cells, repetitions):
    groups = {}
    for label in LABELS:
        for r2 in R2_STATES:
            selected = [cell for cell in cells if cell["metadata"]["label"] == label and cell["metadata"]["r2"] == r2]
            selected.sort(key=lambda cell: cell["metadata"]["rep"])
            complete = [cell for cell in selected if cell["summary"]["complete"]]
            by_rep = {cell["metadata"]["rep"]: cell for cell in selected}
            rep_medians = []
            for rep in range(1, repetitions + 1):
                cell = by_rep.get(rep)
                if cell is None or cell["summary"].get("setup_error"):
                    rep_medians.append(None)
                else:
                    rep_medians.append(cell["summary"]["metrics"]["total_ms"]["median"])
            all_complete = len(complete) == repetitions
            records = (
                [
                    record
                    for cell in complete
                    for record in cell["records"]
                    if record["ok"]
                ]
                if all_complete
                else []
            )
            metrics = phase_metrics(records) if records else empty_metrics()
            total_ns = (
                sum(record["total_ns"] for record in records)
                if records and all(record.get("total_ns") is not None for record in records)
                else None
            )
            total_bytes = sum(record["size_bytes"] for record in records) if records else None
            reasons = {}
            for record in records:
                for phase, reason in record.get("phase_unavailable", {}).items():
                    reasons.setdefault(phase, {}).setdefault(reason, 0)
                    reasons[phase][reason] += 1
            groups[f"{label}-r2{r2}"] = {
                "label": label,
                "r2": r2,
                "repetitions_expected": repetitions,
                "repetitions_complete": len(complete),
                "complete": all_complete,
                "phases_available": bool(records)
                and all(record["phases_available"] for record in records),
                "rep_medians_total_ms": rep_medians,
                "pooled_total_ms": metrics["total_ms"],
                "total_ms": None if total_ns is None else round(total_ns / 1e6, 3),
                "total_bytes": total_bytes,
                "throughput_mb_s": (
                    round(total_bytes * 1000 / total_ns, 6) if total_ns else None
                ),
                "pooled_add_ms": metrics["add_ms"],
                "pooled_commit_ms": metrics["commit_ms"],
                "phase_stats": metrics,
                "phase_shares_pct": phase_shares(records) if records else {
                    phase: metric_stats([]) for phase in PHASES
                },
                "phase_parent": NESTED_PHASES,
                "phase_unavailable": reasons,
                "cells": [
                    {
                        "rep": cell["metadata"]["rep"],
                        "branch": cell["metadata"]["branch"],
                        "namespace": cell["metadata"]["namespace"],
                        "complete": cell["summary"]["complete"],
                        "phases_available": cell["summary"].get("phases_available", False),
                        "succeeded": cell["summary"]["succeeded"],
                        "failed": cell["summary"]["failed"],
                        "aborted_at": cell["summary"]["aborted_at"],
                        "total_ms": cell["summary"]["metrics"]["total_ms"],
                    }
                    for cell in selected
                ],
            }
    return groups


def compare_groups(groups):
    def mean(key):
        group = groups[key]
        return group["pooled_total_ms"]["mean"] if group["complete"] else None

    def delta(first, second):
        return None if first is None or second is None else round(first - second, 3)

    comparisons = {}
    for label in LABELS:
        comparisons[f"{label}_r2_overhead_ms_per_image"] = delta(
            mean(f"{label}-r2on"), mean(f"{label}-r2off")
        )
    for state in R2_STATES:
        old = mean(f"old-r2{state}")
        new = mean(f"new-r2{state}")
        improvement = delta(old, new)
        comparisons[f"uuid_r2{state}_improvement_ms_per_image"] = improvement
        comparisons[f"uuid_r2{state}_improvement_pct"] = (
            None if improvement is None or not old else round(100 * improvement / old, 3)
        )
    return comparisons


def write_group_csv(path, groups):
    fields = [
        "group",
        "label",
        "r2",
        "complete",
        "repetitions_complete",
        "phase",
        "parent",
        "count",
        "mean_ms",
        "median_ms",
        "p95_ms",
        "min_ms",
        "max_ms",
        "share_of_total_mean_pct",
        "share_of_total_median_pct",
    ]
    with open(path, "w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        for key in (f"{label}-r2{r2}" for label in LABELS for r2 in R2_STATES):
            group = groups[key]
            rows = [("total", None)]
            rows.extend((phase, NESTED_PHASES.get(phase)) for phase in PHASES)
            for phase, parent in rows:
                stats = group["phase_stats"][phase + "_ms"]
                share = group["phase_shares_pct"].get(
                    phase, {"mean": None, "median": None}
                )
                if phase == "total" and stats["mean"] is not None:
                    share = {"mean": 100.0, "median": 100.0}
                writer.writerow(
                    {
                        "group": key,
                        "label": group["label"],
                        "r2": group["r2"],
                        "complete": group["complete"],
                        "repetitions_complete": group["repetitions_complete"],
                        "phase": phase,
                        "parent": parent or "",
                        "count": stats["count"],
                        "mean_ms": "" if stats["mean"] is None else stats["mean"],
                        "median_ms": "" if stats["median"] is None else stats["median"],
                        "p95_ms": "" if stats["p95"] is None else stats["p95"],
                        "min_ms": "" if stats["min"] is None else stats["min"],
                        "max_ms": "" if stats["max"] is None else stats["max"],
                        "share_of_total_mean_pct": ""
                        if share.get("mean") is None
                        else round(share["mean"], 3),
                        "share_of_total_median_pct": ""
                        if share.get("median") is None
                        else round(share["median"], 3),
                    }
                )


def write_matrix_outputs(out_dir, stamp, metadata, cells, groups):
    base = unique_base(out_dir, f"upload-benchmark-{stamp}")
    comparisons = compare_groups(groups)
    payload = {
        "benchmark": metadata,
        "groups": groups,
        "comparisons": comparisons,
        "cells": [
            {
                "metadata": cell["metadata"],
                "summary": cell["summary"],
                "records_file": cell["metadata"].get("raw_jsonl"),
            }
            for cell in cells
        ],
    }
    write_json(base + ".json", payload)
    write_group_csv(base + "-groups.csv", groups)
    webp = metadata["webp"]
    lines = [
        f"# Upload benchmark matrix: {stamp}",
        "",
        f"- repo: `{metadata['repo']}`",
        f"- images: {metadata['image_count']} ({metadata['total_bytes']} bytes), expected {metadata['expected_images']}",
        f"- identical files: {metadata['corpus']['duplicate_file_count']} duplicate(s) in "
        f"{len(metadata['corpus']['duplicate_groups'])} group(s); the dataset was not changed",
        "- object-work caveat: old SHA-addressed paths can reuse one image blob for identical bytes; "
        "new asset-UUID paths publish one blob per asset, so the two builds do not perform identical work",
        f"- WebP conversion: {'enabled' if webp['convert'] else 'disabled'} for both builds"
        + ("" if webp["convert"] else " (`--webp=false`); the webp phase is therefore unavailable"),
        f"- repetitions per group: {metadata['repetitions']}",
        f"- R2 mode: `{metadata['r2']['mode']}`; endpoint/bucket: "
        f"`{metadata['r2']['endpoint']}` / `{metadata['r2']['bucket']}`",
        f"- R2 private-bucket confirmation: {metadata['r2']['private_bucket_confirmed']}; "
        f"public base: `{metadata['r2']['public_base_url']}`; "
        f"probe: `{json.dumps(metadata['r2']['probe'], ensure_ascii=False)}`",
        (
            f"- published proof: `{metadata['r2']['published_mapping']['proof']}` "
            f"(mapping sha256 `{metadata['r2']['published_mapping']['mapping_sha256']}`, "
            f"{metadata['r2']['published_mapping']['unique_contents']} unique contents, "
            f"{metadata['r2']['published_mapping']['unique_content_bytes']} bytes re-fetched and verified)"
            if metadata["r2"].get("published_mapping")
            else "- published proof: not used (private dedicated-bucket mode)"
        ),
        f"- binary revisions: "
        + ", ".join(
            f"{label}={entry['vcs_revision'] or 'unknown'}"
            for label, entry in metadata["binaries"].items()
        ),
        f"- R2 isolation: unique namespace per cell, objects under `objects/<namespace>/`; no deletes were issued",
        f"- R2 ownership inventory: `{metadata['r2']['inventory']}`; "
        + (
            "never delete the production bucket; remove only the exact listed object paths"
            if metadata["r2"]["mode"] == "published"
            else "delete the temporary bucket, not a generic prefix"
        )
        + ". R2 conditional DELETE cannot guarantee atomicity; this harness never deletes objects.",
        "- timing: Go `time.Since` spans inside the staged add and published commit; no measured retries",
        "- measured code: runners built from instrumented temporary copies of the listed sources; "
        "the supplied binaries are used only for untimed setup",
        "- phase buckets: ingest/metadata/install_rest/verify/save (add), "
        "git_prepare/upload/commit_git/plan/journal/finalize/r2/cleanup (commit), "
        "plus total and other (CLI residual). decode/webp/hash are nested in ingest; "
        "verify_hash is nested in verify; upload_hash is nested in upload; total_hash sums the "
        "image-byte hash writes and is nested/non-additive; r2_put/r2_ingest/r2_hash are nested in r2.",
        "- hash caveat: the hash aggregates count measured SHA-256 writes over image bytes only; "
        "sha256.New/Sum, metadata hashes and SDK signing are excluded.",
        "",
        "| group | R2 | complete reps | images | total ms | mean ms/image | median ms/image | p95 ms/image | MB/s |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for key in (f"{label}-r2{r2}" for label in LABELS for r2 in R2_STATES):
        group = groups[key]
        images = sum(cell["succeeded"] for cell in group["cells"])
        expected_images = metadata["expected_images"] * metadata["repetitions"]
        stats = group["pooled_total_ms"]
        lines.append(
            f"| {group['label']} | {group['r2']} | {group['repetitions_complete']}/{group['repetitions_expected']} | "
            f"{images}/{expected_images} | {fmt(group['total_ms'])} | {fmt(stats['mean'])} | "
            f"{fmt(stats['median'])} | {fmt(stats['p95'])} | {fmt(group['throughput_mb_s'])} |"
        )
    lines.append("")
    lines.append("## Comparisons")
    lines.append("")
    lines.append("Positive R2 overhead is slower; positive UUID improvement is faster.")
    lines.append("Values are differences in pooled mean milliseconds per image, not causal attribution.")
    lines.append("")
    for label in LABELS:
        key = f"{label}_r2_overhead_ms_per_image"
        lines.append(f"- {label} R2 overhead: {fmt(comparisons[key])} ms/image")
    for state in R2_STATES:
        key = f"uuid_r2{state}_improvement"
        lines.append(
            f"- UUID improvement with R2 {state}: {fmt(comparisons[key + '_ms_per_image'])} "
            f"ms/image ({fmt(comparisons[key + '_pct'])}%)"
        )
    lines.append("")
    lines.append("## Phase medians (ms, share of total in parentheses)")
    lines.append("")
    lines.append(
        "| phase | old / R2 off | old / R2 on | new / R2 off | new / R2 on |"
    )
    lines.append("| --- | ---: | ---: | ---: | ---: |")
    group_keys = [f"{label}-r2{r2}" for label in LABELS for r2 in R2_STATES]
    for phase in PHASES:
        cells_text = []
        for key in group_keys:
            group = groups[key]
            stats = group["phase_stats"][phase + "_ms"]
            share = group["phase_shares_pct"].get(phase, {})
            if not group["complete"] or stats["median"] is None:
                cells_text.append("n/a")
            elif share.get("median") is None:
                cells_text.append(fmt(stats["median"]))
            else:
                cells_text.append(f"{fmt(stats['median'])} ({fmt(share['median'], 1)}%)")
        label = f"{phase}*" if phase in NESTED_PHASES else phase
        lines.append(f"| {label} | " + " | ".join(cells_text) + " |")
    lines.append("")
    lines.append(
        "`*` nested phase: its share overlaps the parent phase and is not additive. "
        "The exclusive buckets plus `other` sum to the per-image total."
    )
    lines.append("")
    lines.append("## Unavailable phases")
    lines.append("")
    for key in group_keys:
        reasons = groups[key]["phase_unavailable"]
        if not reasons:
            continue
        detail = "; ".join(
            f"{phase}: {' / '.join(f'{reason} ({count})' for reason, count in sorted(values.items()))}"
            for phase, values in sorted(reasons.items())
        )
        lines.append(f"- **{key}**: {detail}")
    lines.append("")
    lines.append(f"- group CSV: `{base}-groups.csv` (one row per group and phase)")
    lines.append(
        "Only groups with every repetition complete are compared. An incomplete cell stops at its "
        "first failed add or commit, keeps partial raw data and is excluded from pooled statistics."
    )
    lines.append("")
    with open(base + ".md", "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))
    return base


def unique_base(directory, stem):
    suffixes = (".json", ".jsonl", ".csv", ".md")
    base = os.path.join(directory, stem)
    if not any(os.path.exists(base + suffix) for suffix in suffixes):
        return base
    counter = 1
    while True:
        candidate = f"{base}-{counter}"
        if not any(os.path.exists(candidate + suffix) for suffix in suffixes):
            return candidate
        counter += 1


def run_hash_bench(runner, runner_meta, images, out_dir, stamp, label, repeats, timeout):
    command = [runner, "-hash-dir", os.path.dirname(images[0]), "-hash-repeats", str(repeats)]
    proc = run_checked(command, command_env(), timeout, "SHA-256 microbenchmark")
    try:
        result = json.loads(proc.stdout)
    except json.JSONDecodeError as error:
        die(f"hash microbenchmark returned unreadable JSON: {error}")
    samples = result.get("samples", [])
    per_repeat = {}
    for sample in samples:
        repeat = sample.get("repeat")
        bucket = per_repeat.setdefault(
            repeat, {"repeat": repeat, "files": 0, "bytes": 0, "hash_ns": 0}
        )
        bucket["files"] += 1
        bucket["bytes"] += int(sample.get("size_bytes", 0))
        bucket["hash_ns"] += int(sample.get("hash_ns", 0))
    repeat_rows = []
    for repeat in sorted(per_repeat):
        bucket = per_repeat[repeat]
        seconds = bucket["hash_ns"] / 1e9
        bucket["hash_ms"] = round(bucket["hash_ns"] / 1e6, 3)
        bucket["mb_s"] = round(bucket["bytes"] / 1e6 / seconds, 3) if seconds > 0 else None
        repeat_rows.append(bucket)
    result["per_repeat"] = repeat_rows
    input_bytes = int(result.get("input_bytes_per_repeat", 0))
    if repeat_rows:
        pass_seconds = statistics.fmean(
            bucket["hash_ns"] / 1e9 for bucket in repeat_rows if bucket["hash_ns"] > 0
        )
        result["throughput_mb_s"] = (
            round(input_bytes / 1e6 / pass_seconds, 3) if pass_seconds > 0 else None
        )
    else:
        result["throughput_mb_s"] = None
    result["generated_at"] = datetime.now(timezone.utc).isoformat()
    result["label"] = label
    result["runner"] = runner_meta["runner"]
    result["runner_sha256"] = runner_meta.get("runner_sha256")
    result["source"] = runner_meta.get("source")
    result["source_head"] = runner_meta.get("source_head")
    result["phases"] = ["hash"]
    result["phase_note"] = (
        "the hash phase is measured per file for every repetition after one untimed warmup; "
        "it is not nested in any other measured phase"
    )
    base = unique_base(out_dir, f"hash-microbenchmark-{stamp}-{label}")
    write_json(base + ".json", result)
    fields = ["repeat", "file", "size_bytes", "hash_ns", "hash_ms"]
    with open(base + ".csv", "w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        for sample in samples:
            writer.writerow(
                {
                    "repeat": sample.get("repeat", ""),
                    "file": sample.get("file", ""),
                    "size_bytes": sample.get("size_bytes", ""),
                    "hash_ns": sample.get("hash_ns", ""),
                    "hash_ms": round(sample.get("hash_ns", 0) / 1e6, 6),
                }
            )
    lines = [
        f"# SHA-256 in-memory microbenchmark: {label}",
        "",
        f"{result.get('image_count', 0)} images, {input_bytes} bytes per pass, "
        f"{repeats} measured passes after one untimed warmup; Go `time.Since`.",
        "",
        "Each file is read outside the timer; only the in-memory `crypto/sha256` computation is timed. "
        "This excludes image processing, staging, GitHub and R2, and is **not** upload-stage timing. "
        "The hash phase is fully measured: "
        f"{len(samples)} per-file samples.",
        "",
        "| Metric | Value |",
        "| --- | ---: |",
        f"| Mean | {fmt(result.get('mean_ms'), 6)} ms/image |",
        f"| Median | {fmt(result.get('median_ms'), 6)} ms/image |",
        f"| P95 | {fmt(result.get('p95_ms'), 6)} ms/image |",
        f"| Throughput | {fmt(result.get('throughput_mb_s'))} MB/s |",
        "",
        "| pass | files | bytes | total ms | MB/s |",
        "| ---: | ---: | ---: | ---: | ---: |",
    ]
    for bucket in repeat_rows:
        lines.append(
            f"| {bucket['repeat']} | {bucket['files']} | {bucket['bytes']} | "
            f"{bucket['hash_ms']} | {fmt(bucket['mb_s'])} |"
        )
    lines.extend(
        [
            "",
            f"- runner: `{result['runner']}`",
            f"- source: `{result.get('source')}` at `{result.get('source_head')}`",
            "",
        ]
    )
    with open(base + ".md", "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))
    print(json.dumps({"hash_microbenchmark": base + ".json"}, ensure_ascii=False))
    return base


def main():
    parser = argparse.ArgumentParser(
        description="Measure the four old/new x R2 on/off upload groups with Go phase timing."
    )
    parser.add_argument("--binary", action="append", default=[], help="LABEL=PATH (old and new)")
    parser.add_argument("--source", action="append", default=[], help="LABEL=DIR to build the Go timing runner")
    parser.add_argument(
        "--webp",
        action="store_true",
        help="pass --webp to add for both builds; without it the webp phase is unavailable",
    )
    parser.add_argument(
        "--allow-dirty-source",
        action="store_true",
        help="allow building the timing runner from a checkout with tracked changes",
    )
    parser.add_argument("--repo", help="dedicated OWNER/REPO for the benchmark")
    parser.add_argument("--base-branch", default="", help="branch to fork each cell from (default: repository default)")
    parser.add_argument("--branch-template", default="benchmark/{stamp}/{label}-r2{r2}-r{rep}")
    parser.add_argument("--create-repo", action="store_true", help="create the private repository with a README if missing")
    parser.add_argument("--no-create-branches", action="store_true", help="use branches that already exist")
    parser.add_argument("--images", required=True, help="directory containing exactly the expected images")
    parser.add_argument("--expect-images", type=int, default=200)
    parser.add_argument("--expect-bytes", type=int, default=0, help="optional exact total byte count")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument(
        "--r2-prefix",
        default="",
        help="unique namespace prefix (default: benchmark-<stamp>; forbidden with --published-mapping)",
    )
    parser.add_argument("--r2-endpoint", default="", help="R2 endpoint; defaults to the workspace value with --published-mapping")
    parser.add_argument("--r2-bucket", default="", help="R2 bucket; defaults to the workspace production bucket only with --published-mapping")
    parser.add_argument(
        "--r2-public-base",
        default=None,
        help="private mode only: non-routable https://*.invalid placeholder (metadata, not privacy)",
    )
    parser.add_argument(
        "--r2-bucket-private",
        action="store_true",
        help="private mode only: confirm the supplied bucket is dedicated to this run and has no public access",
    )
    parser.add_argument(
        "--published-mapping",
        default="",
        help=(
            "migration/url-mapping.json whose rows prove every input file is already public; "
            "enables the workspace production bucket only after every unique content is fetched and verified"
        ),
    )
    parser.add_argument("--credentials", default=os.path.expanduser("~/.local/share/touko/cloudflare/credentials.json"))
    parser.add_argument("--root", default="", help="new or empty root for state and runner builds")
    parser.add_argument("--out-dir", default=os.path.join("benchmarks", "results"))
    parser.add_argument("--timeout", type=int, default=900, help="per operation timeout in seconds")
    parser.add_argument("--hash-bench", action="store_true", help="run the separate SHA-256 microbenchmark after the matrix")
    parser.add_argument("--hash-only", action="store_true", help="run only the SHA-256 microbenchmark")
    parser.add_argument("--hash-repeats", type=int, default=3)
    args = parser.parse_args()

    if args.repetitions < 1:
        die("--repetitions must be at least 1")
    if args.expect_images < 1:
        die("--expect-images must be positive")
    if args.hash_repeats < 1:
        die("--hash-repeats must be at least 1")

    args.images = os.path.abspath(os.path.expanduser(args.images))
    images = image_files(args.images, args.expect_images)
    total_bytes = sum(os.path.getsize(image) for image in images)
    if args.expect_bytes and total_bytes != args.expect_bytes:
        die(f"expected {args.expect_bytes} bytes in {args.images}, found {total_bytes}")
    corpus = analyse_corpus(images)

    binaries = key_values(args.binary, "--binary")
    sources = key_values(args.source, "--source")
    stamp = utc_stamp()

    if args.root:
        root = os.path.abspath(os.path.expanduser(args.root))
        if os.path.exists(root):
            if not os.path.isdir(root):
                die(f"--root is not a directory: {root}")
            if os.listdir(root):
                die(f"--root must be new or empty: {root}")
        else:
            os.makedirs(root, mode=0o700)
    else:
        root = tempfile.mkdtemp(prefix="touko-upload-benchmark.")
    out_dir = os.path.abspath(os.path.expanduser(args.out_dir))
    os.makedirs(out_dir, exist_ok=True)

    env = command_env()
    token_exported = False
    if not env.get("GITHUB_TOKEN"):
        try:
            token = subprocess.run(
                ["gh", "auth", "token", "--hostname", "github.com"],
                capture_output=True,
                text=True,
                timeout=30,
            )
        except (OSError, subprocess.SubprocessError):
            token = None
        if token is not None and token.returncode == 0 and token.stdout.strip():
            env["GITHUB_TOKEN"] = token.stdout.strip()
            token_exported = True
    env.pop("GH_TOKEN", None)

    template = os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "benchmark-upload-go", "main.go.tmpl"
    )
    if not os.path.isfile(template):
        die(f"missing Go runner template: {template}")

    runner_meta = {}
    runner_paths = {}
    build_labels = set(sources) if args.hash_only else set(sources) | set(binaries)
    for label in sorted(build_labels):
        if label not in sources:
            die(f"no --source for label {label!r}")
        path, dirty, instrumentation = build_runner(
            sources[label],
            label,
            root,
            template,
            env,
            max(args.timeout, 1800),
            args.allow_dirty_source,
        )
        runner_paths[label] = path
        runner_meta[label] = {
            "runner": path,
            "runner_sha256": file_sha256(path),
            "source": sources[label],
            "source_head": git_head(sources[label]),
            "source_dirty": dirty,
            "instrumentation": instrumentation,
        }

    if args.hash_only:
        if not runner_paths:
            die("--hash-only requires --source")
        for label in sorted(runner_paths):
            run_hash_bench(
                runner_paths[label],
                runner_meta[label],
                images,
                out_dir,
                stamp,
                label,
                args.hash_repeats,
                args.timeout,
            )
        print(f"root: {root}", flush=True)
        return 0

    if not args.repo:
        die("--repo is required for the four-group matrix")
    if args.repo.count("/") != 1:
        die("--repo must be OWNER/REPO")
    workspace = configured_workspace()
    configured_repo = f"{workspace.get('owner', '')}/{workspace.get('name', '')}"
    if workspace.get("backend") == "github" and args.repo.lower() == configured_repo.lower():
        die("refusing to benchmark against the default workspace's GitHub repository")
    binary_meta = {}
    for label in LABELS:
        if label not in binaries:
            die(f"--binary {label}=PATH is required")
        if not os.path.isfile(binaries[label]):
            die(f"binary for {label} does not exist: {binaries[label]}")
        if label not in runner_paths:
            die(f"--source {label}=DIR is required")
        build = binary_build_info(binaries[label])
        revision = build.get("vcs.revision")
        source_head = runner_meta[label]["source_head"]
        if revision and source_head and revision != source_head:
            die(
                f"binary {label} was built from {revision}, but its source is at {source_head}; "
                "the Go timing runner would measure different code. Use a clean checkout at the "
                "binary revision or rebuild both from the same source."
            )
        if build.get("vcs.modified") == "true":
            print(
                f"warning: binary {label} reports vcs.modified=true; record this caveat",
                file=sys.stderr,
            )
        binary_meta[label] = {
            "path": binaries[label],
            "sha256": file_sha256(binaries[label]),
            "vcs_revision": revision,
            "vcs_modified": build.get("vcs.modified"),
            "vcs_time": build.get("vcs.time"),
        }
    r2_credentials = os.path.abspath(os.path.expanduser(args.credentials))
    if not os.path.isfile(r2_credentials):
        die(f"R2 credentials file not found: {r2_credentials}")
    r2 = resolve_r2_settings(
        workspace,
        bucket=args.r2_bucket,
        endpoint=args.r2_endpoint,
        public_base=args.r2_public_base,
        private_confirmed=args.r2_bucket_private,
        prefix=args.r2_prefix,
        published_mapping=args.published_mapping,
        credentials=r2_credentials,
        images=images,
        timeout=min(args.timeout, 120),
        stamp=stamp,
    )
    r2_prefix = r2["prefix"]
    print(
        f"R2 mode={r2['mode']} bucket={r2['bucket']} prefix={r2_prefix}",
        flush=True,
    )
    published_report = r2.pop("published")
    if published_report:
        proof_base = unique_base(out_dir, f"upload-benchmark-{stamp}-published-proof")
        write_json(proof_base + ".json", published_report)
        r2["published"] = {
            key: value
            for key, value in published_report.items()
            if key != "contents"
        }
        r2["published"]["proof"] = proof_base + ".json"
        print(
            f"published proof: {published_report['unique_contents']} unique content(s), "
            f"{published_report['unique_content_bytes']} bytes -> {proof_base}.json",
            flush=True,
        )
    probe_key = f"objects/{r2_prefix}-probe/zz-benchmark-probe-does-not-exist"
    r2_probe = probe_r2(
        runner_paths["new"],
        r2["endpoint"],
        r2["bucket"],
        r2["public_base_url"],
        r2_credentials,
        probe_key,
        args.timeout,
    )
    base_branch = args.base_branch
    if not args.no_create_branches and not base_branch:
        base_branch = repo_default_branch(args.repo, args.create_repo, args.timeout)
    ensure_private_repo(args.repo, args.timeout)
    if base_branch:
        ensure_clean_base(args.repo, base_branch, args.timeout)

    metadata = {
        "schema": 3,
        "stamp": stamp,
        "repo": args.repo,
        "base_branch": base_branch,
        "images": os.path.abspath(args.images),
        "image_count": len(images),
        "expected_images": args.expect_images,
        "total_bytes": total_bytes,
        "corpus": corpus,
        "webp": {
            "convert": args.webp,
            "note": (
                "both add paths receive the same --webp value; "
                + ("WebP conversion ran and its phase is measured" if args.webp else "no image is re-encoded, so the webp phase is unavailable")
            ),
        },
        "repetitions": args.repetitions,
        "r2": {
            "mode": r2["mode"],
            "endpoint": r2["endpoint"],
            "bucket": r2["bucket"],
            "public_base_url": r2["public_base_url"],
            "prefix": r2_prefix,
            "private_bucket_confirmed": r2["private_bucket_confirmed"],
            "published_mapping": r2.get("published"),
            "probe": r2_probe,
        },
        "binaries": binary_meta,
        "instrumentation": {
            label: runner_meta[label]["instrumentation"] for label in runner_meta
        },
        "phase_definitions": PHASE_DEFINITIONS,
        "nested_phases": NESTED_PHASES,
        "root": root,
        "timing": (
            "Go time.Since spans inside the staged add and published commit; setup excluded; "
            "no measured retries; nested phases are marked and may overlap their parent"
        ),
        "measured_code": (
            "the runners compiled from instrumented temporary copies of the listed sources; "
            "the supplied binaries are used only for untimed repository/state setup"
        ),
        "token_exported": token_exported,
    }

    cells = []
    failures = []
    for label in LABELS:
        for r2_state in R2_STATES:
            for rep in range(1, args.repetitions + 1):
                cell_name = f"{label}-r2{r2_state}-r{rep}"
                namespace = re.sub(r"[^a-z0-9_-]+", "-", f"{r2_prefix}-{label}-{r2_state}-r{rep}").strip("-")
                namespace = namespace[:63].strip("-")
                values = {
                    "stamp": stamp.lower(),
                    "label": label,
                    "r2": r2_state,
                    "rep": rep,
                    "cell": cell_name,
                    "namespace": namespace,
                }
                branch = args.branch_template.format(**values)
                branch = re.sub(r"\s+", "-", branch).strip("/")
                state = os.path.join(root, "state", cell_name)
                base = unique_base(out_dir, f"upload-benchmark-{stamp}-{cell_name}")
                raw_path = base + ".jsonl"
                print(
                    f"=== {cell_name}: repo={args.repo} branch={branch} "
                    f"namespace={namespace} r2={r2_state}",
                    flush=True,
                )
                try:
                    if not args.no_create_branches:
                        create_branch(args.repo, base_branch, branch, args.timeout)
                    configure_state(
                        binaries[label],
                        state,
                        args.repo,
                        branch,
                        state_r2_config(r2_state, r2),
                        env,
                        args.timeout,
                    )
                    records, aborted_at, wall = measure_cell(
                        runner_paths[label],
                        state,
                        images,
                        namespace,
                        cell_name,
                        rep,
                        r2_state,
                        raw_path,
                        env,
                        args.timeout,
                        args.webp,
                    )
                except SystemExit as error:
                    failures.append(f"{cell_name}: {error}")
                    print(f"=== {cell_name}: SETUP FAILED: {error}", flush=True)
                    setup_summary = {
                        "expected_images": len(images),
                        "attempted": 0,
                        "succeeded": 0,
                        "failed": 0,
                        "complete": False,
                        "aborted_at": "setup",
                        "setup_error": str(error),
                        "total_bytes": 0,
                        "phases_available": False,
                        "phases_measured_images": 0,
                        "metrics": empty_metrics(),
                        "throughput_mb_s": None,
                    }
                    setup_metadata = {
                        **metadata,
                        "generated_at": datetime.now(timezone.utc).isoformat(),
                        "label": label,
                        "group": cell_name,
                        "r2": r2_state,
                        "rep": rep,
                        "branch": branch,
                        "namespace": namespace,
                        "r2_object_prefix": f"objects/{namespace}/",
                        "binary": binaries[label],
                        "binary_sha256": file_sha256(binaries[label]),
                        "runner": runner_paths[label],
                        "runner_sha256": runner_meta[label]["runner_sha256"],
                        "source": runner_meta[label]["source"],
                        "source_head": runner_meta[label]["source_head"],
                        "wall_seconds": 0.0,
                        "raw_jsonl": None,
                    }
                    write_setup_failure(base, setup_metadata, setup_summary)
                    cells.append(
                        {"metadata": setup_metadata, "summary": setup_summary, "records": []}
                    )
                    continue

                summary = summarise(records, len(images), aborted_at)
                cell_metadata = {
                    **metadata,
                    "generated_at": datetime.now(timezone.utc).isoformat(),
                    "label": label,
                    "group": cell_name,
                    "r2": r2_state,
                    "rep": rep,
                    "branch": branch,
                    "namespace": namespace,
                    "r2_object_prefix": f"objects/{namespace}/",
                    "binary": binaries[label],
                    "binary_sha256": file_sha256(binaries[label]),
                    "runner": runner_paths[label],
                    "runner_sha256": runner_meta[label]["runner_sha256"],
                    "source": runner_meta[label]["source"],
                    "source_head": runner_meta[label]["source_head"],
                    "wall_seconds": round(wall, 3),
                    "raw_jsonl": raw_path,
                }
                write_cell_outputs(base, cell_metadata, summary, records)
                cell = {
                    "metadata": cell_metadata,
                    "summary": summary,
                    "records": records,
                }
                cells.append(cell)
                print(
                    f"=== {cell_name}: {summary['succeeded']}/{summary['expected_images']} "
                    f"complete={summary['complete']} results={base}.json",
                    flush=True,
                )
                if not summary["complete"]:
                    failures.append(f"{cell_name}: incomplete ({summary['succeeded']}/{summary['expected_images']})")

    if not cells:
        die("no benchmark cell completed; inspect the errors above")
    inventory_base, violations = write_r2_inventory(out_dir, stamp, metadata, cells)
    metadata["r2"]["inventory"] = inventory_base + ".json"
    if violations:
        failures.append(f"{len(violations)} R2 object path(s) escaped their run namespace")
    groups = aggregate_cells(cells, args.repetitions)
    matrix_base = write_matrix_outputs(out_dir, stamp, metadata, cells, groups)
    print(f"matrix: {matrix_base}.json / {matrix_base}.md", flush=True)

    if args.hash_bench:
        for label in sorted(runner_paths):
            run_hash_bench(
                runner_paths[label],
                runner_meta[label],
                images,
                out_dir,
                stamp,
                label,
                args.hash_repeats,
                args.timeout,
            )

    print(f"root: {root}", flush=True)
    if failures:
        print("failures:", file=sys.stderr)
        for failure in failures:
            print(f"- {failure}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
