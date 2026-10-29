#!/usr/bin/env python3
"""Rewrite TOUKO object URLs after a path migration.

Reads migration/url-mapping.json (written by `touko migrate paths`) and replaces
every old object path with the new one. Works for jsDelivr, raw.githubusercontent
and custom Cloudflare/R2 bases because only the repository path is rewritten.
"""

import argparse
import json
import os
import sys

SKIP_DIRS = {".git", "node_modules", "vendor", ".venv", "__pycache__", ".next", "dist", "build"}
SKIP_SUFFIX = {
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tar",
    ".woff", ".woff2", ".ttf", ".otf", ".mp4", ".webm", ".mp3", ".so", ".dylib",
    ".exe", ".bin", ".db", ".sqlite", ".lock",
}


def load_mapping(path):
    data = json.load(open(path))
    mapping = {}
    for item in data:
        old, new = item.get("old_path"), item.get("new_path")
        if old and new and old != new:
            mapping[old] = new
    return mapping


def iter_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [name for name in dirnames if name not in SKIP_DIRS]
        for name in filenames:
            if os.path.splitext(name)[1].lower() in SKIP_SUFFIX:
                continue
            yield os.path.join(dirpath, name)


def rewrite_file(path, mapping):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            text = handle.read()
    except (UnicodeDecodeError, OSError):
        return 0
    changed = 0
    for old, new in mapping.items():
        if old in text:
            changed += text.count(old)
            text = text.replace(old, new)
    if not changed:
        return 0
    if not DRY_RUN:
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(text)
    return changed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mapping", required=True, help="migration/url-mapping.json")
    parser.add_argument("--root", action="append", required=True, help="Directory to rewrite (repeatable)")
    parser.add_argument("--apply", action="store_true", help="Write changes (default is a dry run)")
    args = parser.parse_args()
    global DRY_RUN
    DRY_RUN = not args.apply
    mapping = load_mapping(args.mapping)
    if not mapping:
        raise SystemExit("mapping has no path changes")
    total = 0
    for root in args.root:
        files = 0
        hits = 0
        for path in iter_files(root):
            count = rewrite_file(path, mapping)
            if count:
                files += 1
                hits += count
                print(f"{'would update' if DRY_RUN else 'updated'} {path} ({count})")
        print(f"{root}: {files} files, {hits} references")
        total += hits
    print(f"total references: {total}")
    if not args.apply:
        print("dry run; pass --apply to write")
    return 0


if __name__ == "__main__":
    sys.exit(main())
