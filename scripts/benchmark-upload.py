#!/usr/bin/env python3
"""Measure one-image uploads against a real GitHub repository.

Every image is staged and committed on its own, so the per-image time includes a
full publication. Results are written as JSONL while running and summarised as
JSON, CSV and Markdown under benchmarks/results/.

The script never touches an existing gallery: it requires a fresh state
directory and a repository dedicated to the run. See docs/benchmark.md.
"""

import argparse
import csv
import json
import os
import shutil
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone

FIELDS = ["index", "file", "size_bytes", "add_ms", "commit_ms", "total_ms", "ok", "error"]
R2_BLOCK = {
    "enabled": True,
    "endpoint": "https://14c607504ef688dce7ccad9bbaa5d2c6.r2.cloudflarestorage.com",
    "bucket": "touko-imgs",
    "public_base_url": "https://img.eurekaimer.icu",
}


def run(binary, state, *args, timeout=900):
    started = time.monotonic()
    proc = subprocess.run(
        [binary, "--state-dir", state, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    elapsed = (time.monotonic() - started) * 1000.0
    return proc, elapsed


def configure_state(binary, state, repo, branch, r2, credentials):
    if os.path.exists(os.path.join(state, "config.json")):
        raise SystemExit(f"state already configured: {state}")
    os.makedirs(state, exist_ok=True)
    args = ["repo", "use", repo, "--json"]
    if branch:
        args += ["--branch", branch]
    proc, _ = run(binary, state, *args)
    if proc.returncode != 0:
        raise SystemExit(f"repo use failed: {proc.stderr.strip()}")
    proc, _ = run(binary, state, "repo", "init", "--json")
    if proc.returncode != 0 and "already" not in (proc.stderr + proc.stdout):
        raise SystemExit(f"repo init failed: {proc.stderr.strip()}")
    if r2:
        path = os.path.join(state, "config.json")
        config = json.load(open(path))
        config["cloudflare"] = R2_BLOCK
        json.dump(config, open(path, "w"), ensure_ascii=False, indent=2)
        target = os.path.join(state, "cloudflare")
        os.makedirs(target, mode=0o700, exist_ok=True)
        shutil.copyfile(credentials, os.path.join(target, "credentials.json"))
        os.chmod(os.path.join(target, "credentials.json"), 0o600)
    proc, _ = run(binary, state, "sync", "--json")
    if proc.returncode != 0:
        raise SystemExit(f"sync failed: {proc.stderr.strip()}")


def measure(binary, state, images, out_path):
    records = []
    with open(out_path, "w") as out:
        for index, image in enumerate(images):
            record = {"index": index, "file": os.path.basename(image), "size_bytes": os.path.getsize(image)}
            proc, add_ms = run(binary, state, "add", image, "--namespace", "bench", "--json")
            record["add_ms"] = round(add_ms, 1)
            if proc.returncode != 0:
                record.update(ok=False, commit_ms=0.0, total_ms=round(add_ms, 1), error=proc.stderr.strip()[:300])
            else:
                proc, commit_ms = run(binary, state, "commit", "-m", "bench: upload image", "--json")
                record["commit_ms"] = round(commit_ms, 1)
                record["total_ms"] = round(add_ms + commit_ms, 1)
                record["ok"] = proc.returncode == 0
                record["error"] = "" if record["ok"] else proc.stderr.strip()[:300]
            out.write(json.dumps(record, ensure_ascii=False) + "\n")
            out.flush()
            records.append(record)
            print(
                f"{index + 1:3d}/{len(images)} {record['file']} total={record['total_ms']:.0f}ms"
                + ("" if record["ok"] else f" FAILED {record['error']}"),
                flush=True,
            )
            if not record["ok"]:
                break
    return records


def percentile(values, fraction):
    if not values:
        return 0.0
    ordered = sorted(values)
    position = max(0, min(len(ordered) - 1, int(round(fraction * (len(ordered) - 1)))))
    return ordered[position]


def summarise(group, records, wall_seconds):
    ok = [r for r in records if r["ok"]]
    totals = [r["total_ms"] for r in ok]
    commits = [r["commit_ms"] for r in ok]
    bytes_total = sum(r["size_bytes"] for r in ok)
    summary = {
        "group": group,
        "count": len(ok),
        "failed": len(records) - len(ok),
        "total_bytes": bytes_total,
        "total_ms": round(sum(totals), 1),
        "wall_seconds": round(wall_seconds, 2),
        "mean_ms": round(statistics.fmean(totals), 1) if totals else 0.0,
        "median_ms": round(statistics.median(totals), 1) if totals else 0.0,
        "p95_ms": round(percentile(totals, 0.95), 1) if totals else 0.0,
        "mean_commit_ms": round(statistics.fmean(commits), 1) if commits else 0.0,
        "throughput_mb_s": round(bytes_total / 1e6 / (sum(totals) / 1000.0), 3) if totals else 0.0,
    }
    return summary


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--group", required=True, help="Label stored with the results")
    parser.add_argument("--repo", required=True, help="OWNER/REPO dedicated to this run")
    parser.add_argument("--branch", default="", help="Branch dedicated to this group (default: repository default)")
    parser.add_argument("--images", required=True, help="Directory of images")
    parser.add_argument("--state", required=True, help="Fresh state directory")
    parser.add_argument("--r2", choices=["on", "off"], default="off")
    parser.add_argument("--credentials", default=os.path.expanduser("~/.local/share/touko/cloudflare/credentials.json"))
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--out-dir", default=os.path.join("benchmarks", "results"))
    parser.add_argument("--json-only", action="store_true")
    args = parser.parse_args()

    images = sorted(
        os.path.join(args.images, name)
        for name in os.listdir(args.images)
        if name.lower().endswith((".png", ".jpg", ".jpeg", ".gif", ".webp"))
    )
    if args.limit:
        images = images[: args.limit]
    if not images:
        raise SystemExit("no images found")

    configure_state(args.binary, args.state, args.repo, args.branch, args.r2 == "on", args.credentials)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    os.makedirs(args.out_dir, exist_ok=True)
    base = os.path.join(args.out_dir, f"upload-benchmark-{stamp}-{args.group}")
    raw_path = base + ".jsonl"

    started = time.monotonic()
    records = measure(args.binary, args.state, images, raw_path)
    wall = time.monotonic() - started
    summary = summarise(args.group, records, wall)
    summary.update(
        {
            "started_at": stamp,
            "binary": args.binary,
            "repo": args.repo,
            "branch": args.branch,
            "r2": args.r2,
            "images": args.images,
            "raw_jsonl": raw_path,
        }
    )
    with open(base + ".json", "w") as out:
        json.dump({"summary": summary, "records": records}, out, ensure_ascii=False, indent=2)
    with open(base + ".csv", "w", newline="") as out:
        writer = csv.DictWriter(out, fieldnames=FIELDS)
        writer.writeheader()
        writer.writerows(records)
    if not args.json_only:
        with open(base + ".md", "w") as out:
            out.write(f"# Upload benchmark: {args.group}\n\n")
            out.write(f"- binary: `{summary['binary']}`\n- repo: `{summary['repo']}`\n- R2: {summary['r2']}\n")
            out.write(f"- images: {summary['count']} ({summary['total_bytes']} bytes)\n")
            out.write(f"- failed: {summary['failed']}\n\n")
            out.write("| metric | value |\n| --- | ---: |\n")
            for key in ("mean_ms", "median_ms", "p95_ms", "mean_commit_ms", "throughput_mb_s", "total_ms", "wall_seconds"):
                out.write(f"| {key} | {summary[key]} |\n")
    print(json.dumps(summary, ensure_ascii=False))


if __name__ == "__main__":
    sys.exit(main())
