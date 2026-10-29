#!/usr/bin/env python3
"""Rewrite TOUKO object URLs after a path migration.

The mapping written by ``touko migrate paths`` anchors every old path to the
repository and branch (or R2 base) it came from.  Only URLs under those exact
anchors are considered, so an identical path in an unrelated repository is
never rewritten.  The default is a non-mutating preview; ``--apply`` requires
every changed mapping entry to have status ``migrated`` and writes files
atomically.
"""

import argparse
import json
import os
import re
import stat
import string
import sys
import tempfile
from collections import Counter, defaultdict
from urllib.parse import unquote, urlsplit

# Generated, vendored and VCS internals never contain authored references.
SKIP_DIRS = {
    ".git", ".hg", ".svn", "node_modules", "vendor", "bower_components",
    "third_party", "dist", "build", "out", "target", ".next", ".nuxt",
    ".svelte-kit", ".cache", "__pycache__", ".venv", "venv", "site-packages",
    ".tox", ".mypy_cache", ".pytest_cache", ".ruff_cache", "coverage",
    "Pods", ".terraform", "site", "_site", ".docusaurus", ".vuepress",
    ".astro", "htmlcov", ".eggs", ".touko",
}
OBSIDIAN_VENDOR_DIRS = {"plugins", "themes"}
SKIP_SUFFIXES = (
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico", ".bmp", ".tiff",
    ".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".tar", ".woff",
    ".woff2", ".ttf", ".otf", ".eot", ".mp4", ".webm", ".mov", ".mkv", ".mp3",
    ".flac", ".ogg", ".wav", ".so", ".dylib", ".dll", ".exe", ".bin", ".o",
    ".a", ".class", ".jar", ".pyc", ".pyo", ".wasm", ".db", ".sqlite",
    ".sqlite3", ".lock", ".map", ".min.js", ".min.css", ".bundle.js",
)
# Characters net/url.PathEscape leaves alone in a path segment.
_SAFE = frozenset(string.ascii_letters + string.digits + "-._~$&+:=@/")
_URL_RE = re.compile(r"""https?://[^\s<>"'`\\]+""", re.IGNORECASE)


class MappingError(Exception):
    pass


def escape_path(path):
    out = []
    for char in path:
        for byte in char.encode("utf-8"):
            text = chr(byte)
            out.append(text if text in _SAFE else "%%%02X" % byte)
    return "".join(out)


def valid_path(path):
    if not path or path.startswith("/") or path.endswith("/"):
        return False
    if "\\" in path or any(char in path for char in "\x00\n\r"):
        return False
    return all(part not in ("", ".", "..") for part in path.split("/"))


def _trim_url(raw):
    """Separate trailing prose punctuation from a URL token."""
    end = len(raw)
    closers = {")": "(", "]": "[", "}": "{"}
    openers = {"(": ")", "[": "]", "{": "}"}
    while end:
        char = raw[end - 1]
        if char in ".,;:!?":
            end -= 1
        elif char in closers and raw[:end].count(closers[char]) < raw[:end].count(char):
            end -= 1
        elif char in openers and raw[:end].count(char) > raw[:end].count(openers[char]):
            end -= 1
        else:
            break
    return raw[:end], raw[end:]


def _decode_segments(encoded):
    try:
        segments = encoded.split("/")
        if not segments or "" in segments:
            return None
        values = [unquote(segment, errors="strict") for segment in segments]
        if any("/" in value for value in values):
            return None
        return "/".join(values)
    except (UnicodeDecodeError, ValueError):
        return None


def _github_anchors(raw_url):
    """Return (host, path-prefix) anchors for one recorded GitHub URL."""
    try:
        parsed = urlsplit(raw_url)
    except ValueError:
        return []
    host = parsed.netloc.lower()
    parts = parsed.path.lstrip("/").split("/")
    if host == "cdn.jsdelivr.net" and len(parts) >= 3 and parts[0] == "gh" and "@" in parts[2]:
        owner, repo_branch = parts[1], parts[2]
        repo, branch = repo_branch.rsplit("@", 1)
    elif host == "raw.githubusercontent.com":
        if len(parts) >= 5 and parts[2:4] == ["refs", "heads"]:
            owner, repo, branch = parts[0], parts[1], parts[4]
        elif len(parts) >= 4:
            owner, repo, branch = parts[0], parts[1], parts[2]
        else:
            return []
    else:
        return []
    if not owner or not repo or not branch:
        return []
    return [
        ("raw.githubusercontent.com", "/%s/%s/refs/heads/%s/" % (owner, repo, branch)),
        ("raw.githubusercontent.com", "/%s/%s/%s/" % (owner, repo, branch)),
        ("cdn.jsdelivr.net", "/gh/%s/%s@%s/" % (owner, repo, branch)),
    ]


def _r2_anchor(raw_url, old_path):
    try:
        parsed = urlsplit(raw_url)
    except ValueError:
        return None
    suffix = "/" + escape_path(old_path)
    if not parsed.path.endswith(suffix):
        return None
    base = parsed.path[: -len(suffix)].rstrip("/")
    return (parsed.netloc.lower(), base + "/" if base else "/")


def load_mapping(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError) as exc:
        raise MappingError("cannot read mapping %s: %s" % (path, exc))
    if isinstance(data, dict) and isinstance(data.get("items"), list):
        data = data["items"]
    if not isinstance(data, list):
        raise MappingError("mapping must be a JSON array of objects")

    prefixes = defaultdict(dict)  # (host, prefix) -> {old path: new path}
    proofs = {}
    resolved = set()
    ambiguous = defaultdict(set)
    unverified = 0
    skipped = 0
    for index, item in enumerate(data):
        if not isinstance(item, dict):
            raise MappingError("mapping entry %d is not an object" % index)
        old = item.get("old_path") or ""
        new = item.get("new_path") or ""
        status = (item.get("status") or "").strip()
        if status == "failed":
            skipped += 1
            unverified += 1
            continue
        if not isinstance(old, str) or not isinstance(new, str) or not old or not new:
            raise MappingError("mapping entry %d is missing a path" % index)
        if old == new:
            continue
        if not valid_path(old) or not valid_path(new):
            raise MappingError("mapping entry %d has an unsafe path: %r -> %r" % (index, old, new))
        anchors = _github_anchors(item.get("old_github_url") or "")
        r2 = _r2_anchor(item.get("old_r2_url") or "", old)
        if r2:
            anchors.append(r2)
        if not anchors:
            raise MappingError("mapping entry %d has no GitHub or R2 URL to anchor %r" % (index, old))
        if status != "migrated":
            unverified += 1
        proof = (item.get("sha256"), item.get("size_bytes"), status)
        for anchor in anchors:
            target = new
            previous = prefixes[anchor].get(old)
            if previous is not None and previous != target:
                same_bytes = (
                    proof == proofs[(anchor, old)]
                    and status == "migrated"
                    and isinstance(proof[0], str)
                    and re.fullmatch(r"[0-9a-f]{64}", proof[0])
                    and isinstance(proof[1], int)
                    and proof[1] > 0
                )
                if same_bytes:
                    target = min(previous, target)
                    resolved.add((old, target))
                else:
                    ambiguous[anchor].add((old, previous))
                    ambiguous[anchor].add((old, target))
            prefixes[anchor][old] = target
            proofs[(anchor, old)] = proof

    if ambiguous:
        lines = []
        for (host, prefix), values in sorted(ambiguous.items()):
            by_old = defaultdict(set)
            for old, new in values:
                by_old[old].add(new)
            for old, targets in sorted(by_old.items()):
                lines.append("  %s%s%s -> %s" % (host, prefix, old, ", ".join(sorted(targets))))
        raise MappingError("ambiguous one-to-many mapping:\n" + "\n".join(lines))
    for old, new in sorted(resolved):
        print("shared verified object %s: using %s for existing links" % (old, new), file=sys.stderr)
    if not prefixes:
        raise MappingError("mapping has no usable path changes")
    return prefixes, unverified, skipped


def rewrite_text(text, prefixes, stats):
    ordered = sorted(prefixes.items(), key=lambda item: -len(item[0][1]))

    def replace(match):
        raw = match.group(0)
        main, trailing = _trim_url(raw)
        cut = len(main)
        for separator in "?#":
            position = main.find(separator)
            if position != -1 and position < cut:
                cut = position
        url_part, suffix = main[:cut], main[cut:] + trailing
        try:
            parsed = urlsplit(url_part)
        except ValueError:
            return raw
        if parsed.scheme.lower() not in ("http", "https"):
            return raw
        host = parsed.netloc.lower()
        for (anchor_host, prefix), changes in ordered:
            if host != anchor_host or not parsed.path.startswith(prefix):
                continue
            decoded = _decode_segments(parsed.path[len(prefix):])
            if decoded is None:
                return raw
            target = changes.get(decoded)
            if target is None:
                if decoded.startswith("objects/"):
                    stats["links"] += 1
                    stats["unmatched"] += 1
                return raw
            stats["links"] += 1
            stats["matched"] += 1
            stats["changes"] += 1
            rewritten = url_part[: len(url_part) - len(parsed.path) + len(prefix)]
            return rewritten + escape_path(target) + suffix
        return raw

    return _URL_RE.sub(replace, text)


def iter_files(root):
    root = os.path.abspath(root)
    if os.path.isfile(root):
        yield root
        return
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        relative = os.path.relpath(dirpath, root).split(os.sep)
        parts = [] if relative == ["."] else relative
        dirnames[:] = sorted(
            name for name in dirnames
            if name not in SKIP_DIRS
            and not name.endswith(".egg-info")
            and not (".obsidian" in parts and name in OBSIDIAN_VENDOR_DIRS)
        )
        for name in sorted(filenames):
            if name == ".env" or name.startswith(".env.") or name.lower().endswith(SKIP_SUFFIXES) or name.endswith(".egg-info"):
                continue
            path = os.path.join(dirpath, name)
            if os.path.islink(path) or not os.path.isfile(path):
                continue
            relative_file = os.path.relpath(path, root).split(os.sep)
            if any(
                relative_file[index] == "benchmarks" and relative_file[index + 1] == "results"
                for index in range(len(relative_file) - 1)
            ):
                continue
            yield path


def read_text(path):
    try:
        info = os.stat(path, follow_symlinks=False)
        if not stat.S_ISREG(info.st_mode):
            return None, None
        with open(path, "rb") as handle:
            head = handle.read(8192)
            if b"\x00" in head:
                return None, None
            data = head + handle.read()
    except OSError:
        return None, None
    try:
        return data.decode("utf-8"), info
    except UnicodeDecodeError:
        return None, None


def write_atomic(path, text, info):
    current = os.stat(path, follow_symlinks=False)
    before = (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
    after = (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns, current.st_ctime_ns)
    if not stat.S_ISREG(current.st_mode) or before != after:
        raise OSError("file changed while preparing URL rewrite")
    directory = os.path.dirname(path) or "."
    fd, temporary = tempfile.mkstemp(prefix=".touko-rewrite-", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, stat.S_IMODE(info.st_mode))
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def main():
    parser = argparse.ArgumentParser(description="Rewrite mapped TOUKO URLs (dry run by default).")
    parser.add_argument("--mapping", required=True, help="migration/url-mapping.json")
    parser.add_argument("--root", action="append", required=True, help="directory to scan (repeatable)")
    parser.add_argument("--apply", action="store_true", help="write changes; requires migrated entries")
    args = parser.parse_args()
    missing = [root for root in args.root if not os.path.exists(root)]
    if missing:
        print("error: roots do not exist: %s" % ", ".join(missing), file=sys.stderr)
        return 2

    try:
        prefixes, unverified, skipped = load_mapping(args.mapping)
    except MappingError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 2
    if args.apply and unverified:
        print(
            "error: refusing --apply: %d mapping entries are not status=migrated" % unverified,
            file=sys.stderr,
        )
        return 2
    if skipped:
        print("skipped %d failed mapping entries" % skipped)

    totals = Counter()
    failed = []
    mapping_real = os.path.realpath(args.mapping)
    for root in args.root:
        if not os.path.exists(root):
            print("error: root does not exist: %s" % root, file=sys.stderr)
            failed.append(root)
            continue
        stats = Counter()
        files = changed_files = 0
        for path in iter_files(root):
            if os.path.realpath(path) == mapping_real:
                continue
            files += 1
            text, info = read_text(path)
            if text is None:
                continue
            before = stats["changes"]
            updated = rewrite_text(text, prefixes, stats)
            delta = stats["changes"] - before
            if delta <= 0:
                continue
            if args.apply:
                try:
                    write_atomic(path, updated, info)
                except OSError as exc:
                    print("error: could not update %s: %s" % (path, exc), file=sys.stderr)
                    failed.append(path)
                    stats["changes"] -= delta
                    stats["matched"] -= delta
                    stats["links"] -= delta
                    continue
            changed_files += 1
            print("%s %s (%d)" % ("updated" if args.apply else "would update", path, delta))
        print(
            "%s: %d files, %d links, %d matched, %d unmatched, %d references %s"
            % (
                root, files, stats["links"], stats["matched"], stats["unmatched"],
                stats["changes"], "rewritten" if args.apply else "would change",
            )
        )
        totals["files"] += files
        totals["links"] += stats["links"]
        totals["matched"] += stats["matched"]
        totals["unmatched"] += stats["unmatched"]
        totals["changes"] += stats["changes"]
        totals["changed_files"] += changed_files

    print(
        "total: %d files, %d links, %d matched, %d unmatched, %d files %s, %d references %s"
        % (
            totals["files"], totals["links"], totals["matched"], totals["unmatched"],
            totals["changed_files"], "updated" if args.apply else "to update",
            totals["changes"], "rewritten" if args.apply else "would change",
        )
    )
    if not args.apply:
        print("dry run; pass --apply to write")
    if failed:
        print("failed: %s" % ", ".join(failed), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
