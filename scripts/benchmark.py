#!/usr/bin/env python3
"""Measure TOUKO's local CLI and HTTP baseline on reproducible fixtures.

The script builds (or accepts) the real `touko` binary, generates deterministic
PNG fixtures and fresh local Git galleries outside any existing TOUKO state, and
records raw per-sample timings plus medians in `<root>/benchmark.json`.

Every run owns a brand-new root. Fixtures, galleries, states and results stay
there for inspection; nothing is written into an existing gallery or exported
as project source. Python's standard library is the only Python dependency.

Run:

    ROOT=$(mktemp -d "${TMPDIR:-/tmp}/touko-benchmark.XXXXXXXX")
    python3 scripts/benchmark.py --root "$ROOT"

See docs/benchmark.md for metric definitions, the retained layout and the
separate Linux RSS profiling workflow.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import platform
import re
import select
import shlex
import shutil
import signal
import statistics
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zlib
from pathlib import Path

SCHEMA = "touko-benchmark/v1"
PROJECT_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_COUNTS = (0, 100, 1000)
DEFAULT_REPEAT = 3
DEFAULT_BATCH = 10
WIDTH = 256
HEIGHT = 192
SEED = 20261018
PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"
METRICS = (
    "startup_state",
    "http_state",
    "image_bytes",
    "find",
    "reindex",
    "batch_add",
    "commit",
)
CREDENTIAL_ENV = ("GITHUB_TOKEN", "GH_TOKEN", "TOUKO_HOME")
GIT_ISOLATION_ENV = (
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
)
BASELINE_ADD_CHUNK = 100
STARTUP_TIMEOUT = 120.0
HTTP_TIMEOUT = 120.0
COMMAND_TIMEOUT = 1200.0
SHUTDOWN_TIMEOUT = 20.0
HTTP_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class BenchmarkError(RuntimeError):
    """An observable benchmark invariant or subprocess/HTTP call failed."""


def require(condition, message):
    if not condition:
        raise BenchmarkError(message)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def child_environment():
    """Return a child environment without credentials or an ambient TOUKO_HOME."""
    environment = {
        key: value
        for key, value in os.environ.items()
        if key not in CREDENTIAL_ENV and key not in GIT_ISOLATION_ENV
    }
    environment.update(
        {
            "GIT_TERMINAL_PROMPT": "0",
            # Keep builds offline and on the installed toolchain, like examples/demo.sh.
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
            "GONOPROXY": "none",
            # Never route the benchmark's loopback HTTP through a proxy.
            "NO_PROXY": "127.0.0.1,localhost",
            "no_proxy": "127.0.0.1,localhost",
        }
    )
    return environment


def command_output(argv, environment, cwd=None):
    try:
        result = subprocess.run(
            [str(part) for part in argv],
            cwd=str(cwd) if cwd else None,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=COMMAND_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BenchmarkError(f"{shlex.join([str(part) for part in argv])}: {error}") from error
    if result.returncode != 0:
        raise BenchmarkError(
            f"{shlex.join([str(part) for part in argv])} exited {result.returncode}: "
            f"{result.stderr.decode('utf-8', 'replace').strip()}"
        )
    return result.stdout.decode("utf-8", "replace")


def optional_command_output(argv, environment, cwd=None):
    try:
        return command_output(argv, environment, cwd).strip()
    except BenchmarkError:
        return None


def first_line(text):
    if not text:
        return None
    return text.strip().splitlines()[0]


def cpu_model():
    try:
        for line in Path("/proc/cpuinfo").read_text(errors="replace").splitlines():
            if line.lower().startswith("model name"):
                return line.split(":", 1)[1].strip()
    except OSError:
        pass
    return platform.processor() or None


def memory_total_bytes():
    try:
        for line in Path("/proc/meminfo").read_text(errors="replace").splitlines():
            if line.startswith("MemTotal:"):
                return int(line.split()[1]) * 1024
    except (OSError, ValueError, IndexError):
        pass
    return None


def source_metadata(environment):
    """Record revision and a dirty flag without persisting changed file names."""
    revision = optional_command_output(
        ["git", "-C", str(PROJECT_ROOT), "rev-parse", "HEAD"], environment
    )
    status = optional_command_output(
        ["git", "-C", str(PROJECT_ROOT), "status", "--porcelain"],
        environment,
    )
    return {
        "revision": revision,
        "tree_dirty": None if status is None else bool(status.strip()),
        "dirty_scope": "any tracked or untracked change; file names are intentionally not recorded",
    }


def environment_metadata(environment):
    return {
        "os": platform.platform(),
        "kernel": platform.release(),
        "machine": platform.machine(),
        "cpu_model": cpu_model(),
        "cpu_count": os.cpu_count(),
        "memory_bytes": memory_total_bytes(),
        "go": first_line(optional_command_output(["go", "version"], environment)),
        "git": first_line(optional_command_output(["git", "--version"], environment)),
        "python": platform.python_version(),
        "zlib": zlib.ZLIB_VERSION,
        "zlib_runtime": zlib.ZLIB_RUNTIME_VERSION,
        "source": source_metadata(environment),
        "credential_environment_removed": list(CREDENTIAL_ENV),
        "git_environment_removed": list(GIT_ISOLATION_ENV),
        "http_proxy_bypassed": ["127.0.0.1", "localhost"],
    }


def positive_int(text):
    try:
        value = int(text)
    except ValueError as error:
        raise argparse.ArgumentTypeError(f"not an integer: {text}") from error
    if value <= 0:
        raise argparse.ArgumentTypeError("must be greater than zero")
    return value


def parse_counts(text):
    parts = [part.strip() for part in text.split(",")]
    if not parts or any(not part for part in parts):
        raise BenchmarkError("--counts must be a comma-separated list of nonnegative integers")
    try:
        values = [int(part) for part in parts]
    except ValueError as error:
        raise BenchmarkError("--counts must be a comma-separated list of nonnegative integers") from error
    if any(value < 0 for value in values):
        raise BenchmarkError("--counts values must not be negative")
    if len(set(values)) != len(values):
        raise BenchmarkError("--counts values must be unique")
    return sorted(values)


def parse_arguments(argv):
    parser = argparse.ArgumentParser(
        description="Reproducible TOUKO CLI/HTTP baseline on freshly generated galleries.",
        epilog="The root is retained for inspection. See docs/benchmark.md.",
    )
    parser.add_argument(
        "--root",
        required=True,
        help="New or empty directory that owns all fixtures, galleries and results",
    )
    parser.add_argument(
        "--binary",
        help="Existing touko binary to measure (default: build into ROOT/bin/touko)",
    )
    default_counts = ",".join(str(value) for value in DEFAULT_COUNTS)
    parser.add_argument(
        "--counts",
        default=default_counts,
        help=f"Comma-separated committed gallery cardinalities (default: {default_counts})",
    )
    parser.add_argument(
        "--repeat",
        type=positive_int,
        default=DEFAULT_REPEAT,
        help=f"Independent samples per metric (default: {DEFAULT_REPEAT})",
    )
    parser.add_argument(
        "--batch",
        type=positive_int,
        default=DEFAULT_BATCH,
        help=f"Images staged and committed per mutating repetition (default: {DEFAULT_BATCH})",
    )
    parser.add_argument(
        "--prepare-only",
        action="store_true",
        help="Build fixtures and committed galleries, then stop before timing",
    )
    arguments = parser.parse_args(argv)
    try:
        arguments.count_values = parse_counts(arguments.counts)
    except BenchmarkError as error:
        parser.error(str(error))
    return arguments


def prepare_root(raw):
    """Accept only a nonexistent or empty non-symlink directory."""
    path = Path(raw).expanduser()
    if path.is_symlink():
        raise BenchmarkError(f"--root must not be a symlink: {path}")
    if path.exists():
        if not path.is_dir():
            raise BenchmarkError(f"--root must be a directory: {path}")
        try:
            next(path.iterdir())
        except StopIteration:
            pass
        else:
            raise BenchmarkError(f"--root must be empty; refusing to touch {path}")
    else:
        try:
            path.mkdir(parents=True)
        except OSError as error:
            raise BenchmarkError(f"create --root {path}: {error}") from error
    resolved = path.resolve()
    require(resolved.is_dir(), f"--root is not a directory after preparation: {resolved}")
    return resolved


def prepare_binary(root, supplied, environment):
    """Build the real CLI into the fresh root, or validate an explicit binary."""
    if supplied:
        path = Path(supplied).expanduser().resolve()
        require(path.is_file(), f"--binary is not a regular file: {path}")
        require(os.access(path, os.X_OK), f"--binary is not executable: {path}")
        return path, None, False

    if shutil.which("go") is None:
        raise BenchmarkError("go is required to build the CLI; pass --binary to use an existing binary")
    require((PROJECT_ROOT / "go.mod").is_file(), f"go.mod not found under {PROJECT_ROOT}")
    require((PROJECT_ROOT / "cmd" / "touko").is_dir(), f"cmd/touko not found under {PROJECT_ROOT}")
    binary = root / "bin" / "touko"
    binary.parent.mkdir(parents=True, exist_ok=True)
    command = [
        "go",
        "build",
        "-trimpath",
        "-buildvcs=false",
        "-o",
        str(binary),
        "./cmd/touko",
    ]
    print(f"[build] {shlex.join(command)}", flush=True)
    started = time.perf_counter()
    try:
        result = subprocess.run(
            command,
            cwd=str(PROJECT_ROOT),
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=COMMAND_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BenchmarkError(f"build failed: {error}") from error
    elapsed = time.perf_counter() - started
    if result.returncode != 0:
        raise BenchmarkError(
            "build failed: "
            + result.stderr.decode("utf-8", "replace").strip()
        )
    require(binary.is_file(), "go build did not produce a binary")
    return binary, elapsed, True


def binary_metadata(binary):
    stat = binary.stat()
    return {
        "path": str(binary),
        "size_bytes": stat.st_size,
        "sha256": sha256_file(binary),
    }


def png_chunk(kind, payload):
    crc = zlib.crc32(kind + payload) & 0xFFFFFFFF
    return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", crc)


def make_png(seed, index, width, height):
    """Build a deterministic 8-bit RGB PNG whose bytes depend on seed and index."""
    marker = hashlib.sha256(
        f"touko-benchmark-marker:{seed}:{index}".encode("ascii")
    ).digest()
    raw = bytearray()
    for y in range(height):
        digest = hashlib.sha256(
            f"touko-benchmark-v1:{seed}:{index}:{y}".encode("ascii")
        ).digest()
        row = bytearray((digest[0], digest[1], digest[2]) * width)
        if y == 0:
            # An index-specific marker guarantees distinct bytes even if the
            # row-color derivation is ever refactored.
            for offset in range(min(len(marker), len(row))):
                row[offset] = marker[offset]
        raw.append(0)  # PNG filter type: none
        raw.extend(row)
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        PNG_SIGNATURE
        + png_chunk(b"IHDR", header)
        + png_chunk(b"IDAT", zlib.compress(bytes(raw), 6))
        + png_chunk(b"IEND", b"")
    )


def summarize_fixtures(directory, entries):
    digests = {digest for _, digest, _ in entries}
    require(
        len(digests) == len(entries),
        f"fixture generator produced repeated image bytes under {directory}",
    )
    aggregate = hashlib.sha256()
    for name, digest, size in entries:
        aggregate.update(f"{name}\t{digest}\t{size}\n".encode("utf-8"))
    return {
        "path": str(directory),
        "count": len(entries),
        "bytes": sum(size for _, _, size in entries),
        "sha256": aggregate.hexdigest(),
        "first_sha256": entries[0][1] if entries else None,
        "last_sha256": entries[-1][1] if entries else None,
    }


def write_fixture_set(directory, indices, seed, width, height):
    directory.mkdir(parents=True, exist_ok=True)
    entries = []
    for index in indices:
        path = directory / f"bench-{index:06d}.png"
        data = make_png(seed, index, width, height)
        path.write_bytes(data)
        entries.append((path.name, sha256_bytes(data), len(data)))
    return entries


def create_fixtures(root, count, batch, seed, width, height):
    base = root / "datasets" / str(count)
    images = base / "images"
    batch_dir = base / "batch"
    print(f"[fixtures] assets={count} under {base}", flush=True)
    image_entries = write_fixture_set(
        images, range(1, count + 1), seed, width, height
    )
    batch_entries = write_fixture_set(
        batch_dir, range(count + 1, count + batch + 1), seed, width, height
    )
    return {
        "images": summarize_fixtures(images, image_entries),
        "batch": summarize_fixtures(batch_dir, batch_entries),
    }


def cli_call(binary, state, arguments, environment, cwd):
    command = [
        str(binary),
        "--state-dir",
        str(state),
        *[str(argument) for argument in arguments],
        "--json",
    ]
    started = time.perf_counter()
    try:
        result = subprocess.run(
            command,
            cwd=str(cwd),
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=COMMAND_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BenchmarkError(f"{shlex.join(command)}: {error}") from error
    return time.perf_counter() - started, command, result


def decode_json(command, result):
    stdout = result.stdout.decode("utf-8", "replace")
    stderr = result.stderr.decode("utf-8", "replace")
    if result.returncode != 0:
        raise BenchmarkError(
            f"{shlex.join(command)} exited {result.returncode}\n"
            f"stdout: {stdout.strip()}\nstderr: {stderr.strip()}"
        )
    try:
        return json.loads(stdout)
    except json.JSONDecodeError as error:
        raise BenchmarkError(
            f"{shlex.join(command)} did not emit JSON: {error}\nstdout: {stdout[:2000]}"
        ) from error


def cli_json(binary, state, arguments, environment, cwd):
    _, command, result = cli_call(binary, state, arguments, environment, cwd)
    return decode_json(command, result)


def run_git(arguments, environment, cwd):
    command = ["git", *[str(argument) for argument in arguments]]
    try:
        result = subprocess.run(
            command,
            cwd=str(cwd) if cwd else None,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=COMMAND_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BenchmarkError(f"{shlex.join(command)}: {error}") from error
    if result.returncode != 0:
        raise BenchmarkError(
            f"{shlex.join(command)} exited {result.returncode}: "
            f"{result.stderr.decode('utf-8', 'replace').strip()}"
        )
    return result.stdout.decode("utf-8", "replace").strip()


def gallery_head(gallery, environment):
    return run_git(
        ["--git-dir", str(gallery), "rev-parse", "refs/heads/main"], environment, None
    )


def clone_gallery(source, destination, environment):
    destination.parent.mkdir(parents=True, exist_ok=True)
    run_git(
        [
            "clone",
            "--bare",
            "--no-hardlinks",
            "--quiet",
            str(source),
            str(destination),
        ],
        environment,
        None,
    )


def add_baseline_images(binary, state, images, environment, cwd):
    paths = [str(path) for path in images]
    for start in range(0, len(paths), BASELINE_ADD_CHUNK):
        chunk = paths[start : start + BASELINE_ADD_CHUNK]
        added = cli_json(binary, state, ["add", *chunk], environment, cwd)
        require(
            len(added.get("assets", [])) == len(chunk),
            f"baseline add returned {len(added.get('assets', []))} assets for {len(chunk)} files",
        )


def prepare_baseline(binary, root, count, fixture, environment):
    """Create the committed N-asset gallery and a read-only profiler state."""
    base = root / "datasets" / str(count)
    state = base / "state"
    gallery = base / "gallery.git"
    print(f"[baseline] initialize assets={count} under {base}", flush=True)

    initialized = cli_json(
        binary, state, ["repo", "init", "--local", str(gallery)], environment, root
    )
    require(
        initialized.get("asset_count") == 0,
        "baseline init reported nonzero assets",
    )
    initial_head = gallery_head(gallery, environment)
    require(initialized.get("head") == initial_head, "init head differs from Git")

    if count:
        image_paths = sorted((base / "images").glob("*.png"))
        require(len(image_paths) == count, "fixture image count changed before baseline setup")
        add_baseline_images(binary, state, image_paths, environment, root)
        staged = cli_json(binary, state, ["status"], environment, root)
        changes = staged.get("changes", [])
        require(
            len(changes) == count,
            f"baseline staged {len(changes)} changes, expected {count}",
        )
        require(
            staged.get("head") == initial_head and staged.get("base_head") == initial_head,
            "baseline add moved HEAD before commit",
        )
        committed = cli_json(
            binary,
            state,
            ["commit", "-m", f"benchmark: baseline {count} assets"],
            environment,
            root,
        )
        require(
            committed.get("change_count") == count,
            "baseline commit did not publish every staged asset",
        )
        require(committed.get("asset_count") == count, "baseline commit asset count differs")

    head = gallery_head(gallery, environment)
    require(head, "baseline gallery has no main commit")
    if count:
        require(head != initial_head, "baseline commit did not move HEAD")

    status = cli_json(binary, state, ["status"], environment, root)
    require(status.get("changes") == [], "baseline state is not clean")
    require(status.get("head") == head, "baseline state head differs from Git")

    verified = cli_json(binary, state, ["verify"], environment, root)
    require(verified.get("head") == head, "baseline verify head differs")
    require(verified.get("assets") == count, "baseline verify asset count differs")
    require(verified.get("objects") == count, "baseline verify object count differs")

    state_assets = cli_json(binary, state, ["ls"], environment, root).get("assets", [])
    require(len(state_assets) == count, "baseline state listing count differs")
    expected_ids = sorted(view["asset"]["id"] for view in state_assets)

    verify_state = base / "verify-state"
    cli_json(
        binary,
        verify_state,
        ["repo", "use", "--local", str(gallery)],
        environment,
        root,
    )
    synced = cli_json(binary, verify_state, ["sync"], environment, root)
    require(synced.get("head") == head, "fresh baseline sync head differs")
    require(synced.get("asset_count") == count, "fresh baseline sync asset count differs")
    recovered = cli_json(binary, verify_state, ["ls"], environment, root).get("assets", [])
    require(
        sorted(view["asset"]["id"] for view in recovered) == expected_ids,
        "fresh baseline sync recovered different Asset identities",
    )

    return {
        "state": str(state),
        "gallery": str(gallery),
        "verify_state": str(verify_state),
        "head": head,
        "assets": count,
        "objects": count,
        "fixture_images": fixture["images"]["path"],
        "fixture_batch": fixture["batch"]["path"],
    }


def prepare_read_state(binary, base, run_dir, expected, environment, root):
    state = run_dir / "read-state"
    cli_json(
        binary,
        state,
        ["repo", "use", "--local", base["gallery"]],
        environment,
        root,
    )
    synced = cli_json(binary, state, ["sync"], environment, root)
    require(synced.get("head") == base["head"], "read-state sync head differs")
    require(
        synced.get("asset_count") == expected,
        "read-state sync asset count differs",
    )
    return state


def prepare_mutation(binary, base, run_dir, expected, environment, root):
    gallery = run_dir / "gallery.git"
    clone_gallery(base["gallery"], gallery, environment)
    state = run_dir / "mutate-state"
    cli_json(
        binary,
        state,
        ["repo", "use", "--local", str(gallery)],
        environment,
        root,
    )
    synced = cli_json(binary, state, ["sync"], environment, root)
    require(synced.get("head") == base["head"], "mutation clone head differs")
    require(
        synced.get("asset_count") == expected,
        "mutation clone asset count differs",
    )
    head = gallery_head(gallery, environment)
    require(head == base["head"], "mutation gallery head differs from baseline")
    return state, gallery, head


def start_server(binary, state, environment, cwd):
    command = [
        str(binary),
        "--state-dir",
        str(state),
        "serve",
        "--port",
        "0",
        "--no-open",
    ]
    try:
        return subprocess.Popen(
            command,
            cwd=str(cwd),
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except OSError as error:
        raise BenchmarkError(f"{shlex.join(command)}: {error}") from error


def exited_stderr(process):
    if process.stderr is None or process.poll() is None:
        return ""
    try:
        return process.stderr.read().decode("utf-8", "replace").strip()
    except (OSError, ValueError):
        return ""


def read_server_startup(process, deadline, state):
    require(process.stdout is not None, "serve stdout pipe is unavailable")
    descriptor = process.stdout.fileno()
    os.set_blocking(descriptor, False)
    buffer = b""
    while True:
        if process.poll() is not None:
            detail = exited_stderr(process)
            raise BenchmarkError(
                f"serve exited with status {process.returncode} before printing its URL"
                + (f": {detail}" if detail else "")
            )
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise BenchmarkError("serve did not print its loopback URL within the timeout")
        readable, _, _ = select.select([descriptor], [], [], min(0.1, remaining))
        if not readable:
            continue
        try:
            chunk = os.read(descriptor, 65536)
        except BlockingIOError:
            continue
        if not chunk:
            raise BenchmarkError("serve closed stdout before printing its loopback URL")
        buffer += chunk
        require(len(buffer) <= 1_000_000, "serve printed an unexpectedly large startup banner")
        text = buffer.decode("utf-8", "replace")
        url_match = re.search(
            r"^TOUKO Webview: (http://127\.0\.0\.1:\d+)$", text, re.MULTILINE
        )
        state_match = re.search(r"^State: (.+)$", text, re.MULTILINE)
        if url_match and state_match:
            url = url_match.group(1)
            printed_state = Path(state_match.group(1).strip())
            require(
                printed_state.is_absolute(),
                f"serve printed a non-absolute state path: {printed_state}",
            )
            require(
                printed_state.resolve() == Path(state).resolve(),
                f"serve opened {printed_state}, expected {state}",
            )
            port = int(url.rsplit(":", 1)[1])
            require(port > 0, "serve did not select a nonzero loopback port")
            return url


def stop_server(process):
    """Stop only this benchmark's child with SIGTERM, then a bounded kill."""
    killed = False
    if process.poll() is None:
        try:
            process.send_signal(signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=SHUTDOWN_TIMEOUT)
        except subprocess.TimeoutExpired:
            process.kill()
            killed = True
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
    stderr = b""
    if process.stderr is not None:
        try:
            if process.poll() is not None:
                stderr = process.stderr.read()
        except (OSError, ValueError):
            pass
        finally:
            try:
                process.stderr.close()
            except OSError:
                pass
    if process.stdout is not None:
        try:
            process.stdout.close()
        except OSError:
            pass
    if killed:
        raise BenchmarkError("serve did not stop gracefully within the shutdown timeout")
    if process.returncode != 0:
        raise BenchmarkError(
            f"serve exited with status {process.returncode}: "
            f"{stderr.decode('utf-8', 'replace').strip()}"
        )


def stop_server_quietly(process):
    try:
        stop_server(process)
    except Exception:
        pass


def http_read(url):
    try:
        with HTTP_OPENER.open(url, timeout=HTTP_TIMEOUT) as response:
            status = response.status
            headers = {key.lower(): value for key, value in response.headers.items()}
            body = response.read()
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", "replace")[:1000]
        raise BenchmarkError(f"GET {url} returned HTTP {error.code}: {detail}") from error
    except (urllib.error.URLError, OSError) as error:
        raise BenchmarkError(f"GET {url} failed: {error}") from error
    return status, headers, body


def parse_json_body(url, body):
    try:
        return json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise BenchmarkError(f"GET {url} did not return JSON: {error}") from error


def validate_state(payload, expected_assets):
    require(isinstance(payload, dict), "state response is not an object")
    require(payload.get("initialized") is True, "gallery state is not initialized")
    assets = payload.get("assets")
    require(isinstance(assets, list), "state assets is not a list")
    require(
        len(assets) == expected_assets,
        f"state returned {len(assets)} assets, expected {expected_assets}",
    )
    changes = payload.get("changes")
    require(isinstance(changes, list), "state changes is not a list")
    require(changes == [], "committed gallery state unexpectedly has staged changes")
    head = payload.get("head")
    require(isinstance(head, str) and bool(head), "state head is empty")
    for view in assets:
        require(view.get("state") == "committed", "state contains a non-committed asset")
    return assets


def summarize_metric(samples, skipped=None):
    values = [sample["seconds"] for sample in samples]
    metric = {
        "unit": "seconds",
        "n": len(values),
        "samples": samples,
        "median_seconds": statistics.median(values) if values else None,
        "min_seconds": min(values) if values else None,
        "max_seconds": max(values) if values else None,
    }
    if skipped:
        metric["skipped"] = skipped
    return metric


def measure_scenario(binary, root, count, fixture, baseline, repeat, batch, environment):
    scenarios = {metric: [] for metric in METRICS}
    runs = []
    batch_dir = Path(fixture["batch"]["path"])
    batch_paths = sorted(batch_dir.glob("*.png"))
    require(len(batch_paths) == batch, "batch fixture count changed")
    total_after_commit = count + batch
    find_counts = []
    reindex_counts = []
    staged_counts = []
    commit_change_counts = []
    commit_asset_counts = []
    sync_counts = []
    verify_assets = []
    verify_objects = []

    for repetition in range(1, repeat + 1):
        run_dir = root / "runs" / str(count) / f"rep-{repetition:02d}"
        read_state = prepare_read_state(
            binary, baseline, run_dir, count, environment, root
        )
        mutation_state, mutation_gallery, head_before = prepare_mutation(
            binary, baseline, run_dir, count, environment, root
        )
        run_info = {
            "repeat": repetition,
            "read_state": str(read_state),
            "mutation_gallery": str(mutation_gallery),
            "mutation_state": str(mutation_state),
            "head_before": head_before,
        }

        # Cold process startup, then warm HTTP state and original-byte loads on
        # the same process. The startup sample ends at a successful /api/state;
        # all semantic validation happens after the clock stops.
        started = time.perf_counter()
        server = start_server(binary, read_state, environment, root)
        server_ok = False
        try:
            url = read_server_startup(server, started + STARTUP_TIMEOUT, read_state)
            status, _, body = http_read(f"{url}/api/state")
            startup_seconds = time.perf_counter() - started
            require(status == 200, f"startup state returned HTTP {status}")
            startup_state = parse_json_body(f"{url}/api/state", body)
            validate_state(startup_state, count)
            scenarios["startup_state"].append(
                {"seconds": startup_seconds, "url": url, "assets": count}
            )

            state_started = time.perf_counter()
            status, _, body = http_read(f"{url}/api/state")
            state_seconds = time.perf_counter() - state_started
            require(status == 200, f"gallery state returned HTTP {status}")
            state_payload = parse_json_body(f"{url}/api/state", body)
            assets = validate_state(state_payload, count)
            scenarios["http_state"].append(
                {"seconds": state_seconds, "assets": count, "changes": 0}
            )

            if count:
                view = assets[(repetition - 1) % count]
                asset = view["asset"]
                image_url = f"{url}/api/assets/{asset['id']}/image"
                image_started = time.perf_counter()
                status, headers, data = http_read(image_url)
                image_seconds = time.perf_counter() - image_started
                require(status == 200, f"image load returned HTTP {status}")
                require(
                    headers.get("content-type", "").split(";")[0] == asset["object"]["mime"],
                    "image response Content-Type differs from the Asset",
                )
                require(
                    len(data) == asset["object"]["size"],
                    "image response size differs from the Asset",
                )
                digest = sha256_bytes(data)
                require(
                    digest == asset["object"]["hash"],
                    "full image bytes do not match the Asset content hash",
                )
                scenarios["image_bytes"].append(
                    {
                        "seconds": image_seconds,
                        "asset_id": asset["id"],
                        "bytes": len(data),
                        "sha256": digest,
                    }
                )
            server_ok = True
        finally:
            if server_ok:
                stop_server(server)
            else:
                stop_server_quietly(server)

        # Fresh CLI process per find and reindex sample.
        seconds, command, result = cli_call(
            binary, read_state, ["find", "bench"], environment, root
        )
        found = decode_json(command, result)
        found_assets = found.get("assets")
        require(isinstance(found_assets, list), "find did not return an asset list")
        require(
            len(found_assets) == count,
            f"find returned {len(found_assets)} assets, expected {count}",
        )
        require(
            all(view.get("state") == "committed" for view in found_assets),
            "find returned a non-committed Asset",
        )
        find_counts.append(len(found_assets))
        scenarios["find"].append(
            {"seconds": seconds, "query": "bench", "matches": len(found_assets)}
        )

        seconds, command, result = cli_call(
            binary, read_state, ["reindex"], environment, root
        )
        reindexed = decode_json(command, result)
        require(reindexed.get("head") == baseline["head"], "reindex head differs")
        require(
            reindexed.get("asset_count") == count,
            "reindex asset count differs",
        )
        after_reindex = cli_json(binary, read_state, ["status"], environment, root)
        require(after_reindex.get("changes") == [], "reindex changed staged state")
        reindex_counts.append(reindexed.get("asset_count"))
        scenarios["reindex"].append(
            {
                "seconds": seconds,
                "asset_count": reindexed.get("asset_count"),
                "head": reindexed.get("head"),
            }
        )

        # Each mutation starts from an independent clone of the same N baseline.
        seconds, command, result = cli_call(
            binary, mutation_state, ["add", *[str(path) for path in batch_paths]],
            environment, root,
        )
        added = decode_json(command, result)
        require(
            len(added.get("assets", [])) == batch,
            "batch add returned the wrong number of Assets",
        )
        head_after_add = gallery_head(mutation_gallery, environment)
        require(head_after_add == head_before, "batch add moved the published HEAD")
        staged = cli_json(binary, mutation_state, ["status"], environment, root)
        changes = staged.get("changes", [])
        require(len(changes) == batch, "batch add staged the wrong change count")
        require(all(change.get("kind") == "add" for change in changes), "batch add staged a non-add change")
        require(
            staged.get("head") == head_before and staged.get("base_head") == head_before,
            "batch add changed HEAD/base before commit",
        )
        staged_counts.append(len(changes))
        scenarios["batch_add"].append(
            {
                "seconds": seconds,
                "files": batch,
                "staged": len(changes),
                "head_unchanged": True,
            }
        )

        seconds, command, result = cli_call(
            binary,
            mutation_state,
            ["commit", "-m", f"benchmark: add {batch} assets at {count}"],
            environment,
            root,
        )
        committed = decode_json(command, result)
        require(committed.get("change_count") == batch, "commit change count differs")
        require(
            committed.get("asset_count") == total_after_commit,
            "commit asset count differs",
        )
        head_after = gallery_head(mutation_gallery, environment)
        require(committed.get("head") == head_after, "commit result head differs from Git")
        require(head_after != head_before, "commit did not move HEAD")
        after_commit = cli_json(binary, mutation_state, ["status"], environment, root)
        require(after_commit.get("changes") == [], "commit left staged changes")
        scenarios["commit"].append(
            {"seconds": seconds, "changes": batch, "assets": total_after_commit}
        )
        listed = cli_json(binary, mutation_state, ["ls"], environment, root)
        require(
            len(listed.get("assets", [])) == total_after_commit,
            "post-commit listing count differs",
        )

        verifier_state = run_dir / "verifier-state"
        cli_json(
            binary,
            verifier_state,
            ["repo", "use", "--local", str(mutation_gallery)],
            environment,
            root,
        )
        synced = cli_json(binary, verifier_state, ["sync"], environment, root)
        require(synced.get("head") == head_after, "fresh sync head differs after commit")
        require(
            synced.get("asset_count") == total_after_commit,
            "fresh sync asset count differs after commit",
        )
        verified = cli_json(binary, verifier_state, ["verify"], environment, root)
        require(verified.get("assets") == total_after_commit, "fresh verify asset count differs")
        require(verified.get("objects") == total_after_commit, "fresh verify object count differs")
        sync_counts.append(synced.get("asset_count"))
        verify_assets.append(verified.get("assets"))
        verify_objects.append(verified.get("objects"))
        commit_change_counts.append(committed.get("change_count"))
        commit_asset_counts.append(committed.get("asset_count"))

        run_info.update(
            {
                "verifier_state": str(verifier_state),
                "head_after": head_after,
                "staged": len(changes),
                "fresh_sync_assets": synced.get("asset_count"),
            }
        )
        runs.append(run_info)

    metrics = {}
    for metric in METRICS:
        skipped = None
        if metric == "image_bytes" and count == 0:
            skipped = "no committed Assets at cardinality 0"
        require(
            len(scenarios[metric]) == (0 if skipped else repeat),
            f"{metric} did not record every repetition",
        )
        metrics[metric] = summarize_metric(scenarios[metric], skipped)

    return (
        {
            "assets": count,
            "metrics": metrics,
            "medians_seconds": {
                metric: metrics[metric]["median_seconds"] for metric in METRICS
            },
            "checks": {
                "expected_assets": count,
                "expected_after_batch": total_after_commit,
                "fixture_assets": fixture["images"]["count"],
                "fixture_batch": fixture["batch"]["count"],
                "find_matches": find_counts,
                "reindex_asset_counts": reindex_counts,
                "batch_add_staged": staged_counts,
                "add_never_moved_head": True,
                "commit_change_counts": commit_change_counts,
                "commit_asset_counts": commit_asset_counts,
                "commit_cleared_changes": True,
                "fresh_sync_asset_counts": sync_counts,
                "fresh_verify_assets": verify_assets,
                "fresh_verify_objects": verify_objects,
                "image_hashes_verified": len(scenarios["image_bytes"]),
            },
        },
        runs,
    )


def desktop_profiling_metadata(root, baselines):
    """Exact roots and commands for the separate Linux RSS workflow."""
    desktop_binary = root / "bin" / "touko-desktop"
    build = shlex.join(
        [
            "go",
            "build",
            "-trimpath",
            "-buildvcs=false",
            "-o",
            str(desktop_binary),
            "./cmd/touko-desktop",
        ]
    )
    launches = {}
    profilers = {}
    for count, baseline in baselines.items():
        launches[str(count)] = shlex.join(
            [
                str(desktop_binary),
                "--state-dir",
                baseline["state"],
                "--port",
                "0",
            ]
        )
        profilers[str(count)] = {
            phase: shlex.join(
                [
                    "python3",
                    "scripts/desktop-memory.py",
                    "--pid",
                    "PID",
                    "--phase",
                    phase,
                    "--output",
                    str(root / "profiles" / f"{count}.jsonl"),
                    "--samples",
                    "5",
                    "--interval",
                    "0.2",
                ]
            )
            for phase in ("startup", "loaded", "browsed", "committed")
        }
    return {
        "build_desktop": build,
        "state_dirs": {
            str(count): baseline["state"] for count, baseline in baselines.items()
        },
        "launch": launches,
        "profiler_commands": profilers,
        "phases": ["startup", "loaded", "browsed", "committed"],
        "profiler_contract": "Appends one JSONL row per invocation; see scripts/desktop-memory.py.",
        "rss_caveat": (
            "Whole-tree RSS adds shared pages repeatedly; compare per-process values "
            "or use PSS/USS before treating a tree total as additional memory."
        ),
    }


def build_manifest(arguments, root, binary_info, fixtures, baselines, runs, measurements, started):
    prepared_only = bool(arguments.prepare_only)
    return {
        "schema": SCHEMA,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(
            timespec="seconds"
        ),
        "root": str(root),
        "prepared_only": prepared_only,
        "duration_seconds": time.perf_counter() - started,
        "parameters": {
            "counts": arguments.count_values,
            "repeat": arguments.repeat,
            "batch": arguments.batch,
            "fixture_seed": SEED,
            "fixture_width": WIDTH,
            "fixture_height": HEIGHT,
            "prepare_only": prepared_only,
        },
        "environment": environment_metadata(child_environment()),
        "binary": binary_info,
        "fixtures": {
            "generator": {
                "kind": "deterministic stdlib PNG",
                "seed": SEED,
                "width": WIDTH,
                "height": HEIGHT,
                "format": "8-bit RGB, filter 0, zlib level 6",
                "stability": (
                    "The same seed, dimensions and indices produce identical bytes; "
                    "sha256 is an aggregate over name, digest and size."
                ),
            },
            "datasets": {str(count): fixture for count, fixture in fixtures.items()},
        },
        "baselines": {
            str(count): baseline for count, baseline in baselines.items()
        },
        "runs": {
            str(count): runs.get(count, []) for count in arguments.count_values
        },
        "measurements": None
        if prepared_only
        else {str(count): scenario for count, scenario in measurements.items()},
        "desktop_profiling": desktop_profiling_metadata(root, baselines),
        "limitations": [
            "Timings use the host page cache; the benchmark does not drop caches.",
            "HTTP numbers cover loopback transfer and origin byte reads, not browser JavaScript, layout, paint or image decode.",
            "The repository is local Git; GitHub API and network publication latency are out of scope.",
            "Fixtures are synthetic deterministic PNGs, so results describe this corpus.",
            "Medians over --repeat samples are descriptive, not a confidence interval.",
            "Whole-tree RSS double-counts shared pages; renderer absence must not be reported as zero memory.",
            "Only this script's fresh root is writable; existing TOUKO galleries and states are never opened.",
        ],
    }


def write_manifest(path, manifest):
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2)
        handle.write("\n")
    os.replace(temporary, path)


def print_summary(manifest, output, prepared_only):
    print(f"[result] Manifest: {output}", flush=True)
    for count, dataset in manifest["fixtures"]["datasets"].items():
        print(
            f"[result] fixtures assets={count}: images={dataset['images']['path']} "
            f"batch={dataset['batch']['path']}",
            flush=True,
        )
    if prepared_only:
        for count, baseline in manifest["baselines"].items():
            print(
                f"[result] assets={count}: state={baseline['state']} gallery={baseline['gallery']}",
                flush=True,
            )
        return
    for count, scenario in manifest["measurements"].items():
        parts = []
        for metric, data in scenario["metrics"].items():
            median = data["median_seconds"]
            parts.append(
                f"{metric}={median * 1000:.1f}ms" if median is not None else f"{metric}=n/a"
            )
        print(f"[result] assets={count}: " + " ".join(parts), flush=True)
    for count, baseline in manifest["baselines"].items():
        print(
            f"[result] desktop state assets={count}: {baseline['state']}",
            flush=True,
        )


def execute(arguments, root):
    started = time.perf_counter()
    environment = child_environment()
    require(shutil.which("git") is not None, "git is required on PATH")
    binary, build_seconds, built = prepare_binary(
        root, arguments.binary, environment
    )
    binary_info = binary_metadata(binary)
    binary_info.update({"built_by_benchmark": built, "build_seconds": build_seconds})
    print(f"[binary] {binary} sha256={binary_info['sha256']}", flush=True)

    fixtures = {}
    for count in arguments.count_values:
        fixtures[count] = create_fixtures(
            root, count, arguments.batch, SEED, WIDTH, HEIGHT
        )

    baselines = {}
    for count in arguments.count_values:
        baselines[count] = prepare_baseline(
            binary, root, count, fixtures[count], environment
        )

    measurements = {}
    runs = {}
    if not arguments.prepare_only:
        for count in arguments.count_values:
            print(f"[measure] assets={count}", flush=True)
            scenario, scenario_runs = measure_scenario(
                binary,
                root,
                count,
                fixtures[count],
                baselines[count],
                arguments.repeat,
                arguments.batch,
                environment,
            )
            measurements[count] = scenario
            runs[count] = scenario_runs

    manifest = build_manifest(
        arguments,
        root,
        binary_info,
        fixtures,
        baselines,
        runs,
        measurements,
        started,
    )
    output = root / "benchmark.json"
    write_manifest(output, manifest)
    print_summary(manifest, output, arguments.prepare_only)
    print(f"[result] Workspace retained at {root}", flush=True)
    return 0


def main(argv):
    root = None
    try:
        arguments = parse_arguments(argv)
        root = prepare_root(arguments.root)
        print(f"[workspace] {root}", flush=True)
        return execute(arguments, root)
    except KeyboardInterrupt:
        print("benchmark interrupted", file=sys.stderr)
        if root is not None:
            print(f"workspace retained at {root}", file=sys.stderr)
        return 130
    except BenchmarkError as error:
        print(f"benchmark failed: {error}", file=sys.stderr)
        if root is not None:
            print(f"workspace retained at {root}", file=sys.stderr)
        return 1
    except Exception as error:  # an unexpected bug must still name the retained root
        print(f"benchmark failed: {type(error).__name__}: {error}", file=sys.stderr)
        if root is not None:
            print(f"workspace retained at {root}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
