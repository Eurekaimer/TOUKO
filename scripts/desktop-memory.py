#!/usr/bin/env python3
"""Snapshot the RSS of a running touko-desktop and its app-owned process tree.

The target's executable basename must be ``touko-desktop``. Descendants are
found by walking parent links from that one root, so unrelated WebKit processes
owned by other applications or users are never included. WebKit process names
are classified from /proc/PID/exe, not from the 15-byte /proc/PID/comm name.
Only /proc is read; no application endpoint, window, state directory or service
is contacted.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
import time
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Optional

TARGET_EXECUTABLE = "touko-desktop"
PROC_ROOT = "/proc"
PAGE_KIB = os.sysconf("SC_PAGE_SIZE") // 1024
PHASES = ("startup", "loaded", "browsed", "committed")
CATEGORY_KEYS = ("touko-desktop", "WebKitWebProcess", "WebKitNetworkProcess", "other")
WEBKIT_KEYS = ("WebKitWebProcess", "WebKitNetworkProcess")
DELETED_SUFFIX = " (deleted)"


class SamplerError(RuntimeError):
    """A condition that must fail the invocation instead of reporting zero."""


@dataclass(frozen=True)
class ProcStat:
    pid: int
    ppid: int
    starttime: int
    rss_kib: int


@dataclass(frozen=True)
class Sample:
    timestamp: str
    processes: list[dict]
    aggregates: dict
    tree_rss_kib: int


def _read_stat(pid: int) -> Optional[ProcStat]:
    """Read one /proc/PID/stat row, or None if the process already exited.

    PermissionError is deliberately propagated so the root check can turn it
    into a non-zero exit instead of a fabricated zero.
    """
    try:
        with open(f"{PROC_ROOT}/{pid}/stat", "rb") as handle:
            data = handle.read()
    except (FileNotFoundError, ProcessLookupError):
        return None

    left = data.find(b"(")
    right = data.rfind(b")")
    if left < 0 or right <= left:
        return None
    fields = data[right + 2 :].split()
    # After comm, [0] is state (field 3), [1] ppid (field 4),
    # [19] starttime (field 22) and [21] rss pages (field 24).
    if len(fields) < 22:
        return None
    try:
        ppid = int(fields[1])
        starttime = int(fields[19])
        rss_pages = int(fields[21])
    except ValueError:
        return None
    if ppid < 0 or starttime < 0:
        return None
    return ProcStat(pid, ppid, starttime, max(rss_pages, 0) * PAGE_KIB)


def _exe_basename(pid: int) -> Optional[str]:
    """Return the executable basename, stripped of the kernel's deleted mark.

    /proc/PID/exe is authoritative and avoids comm truncation. It does not
    expose command-line arguments or environment data.
    """
    target = os.readlink(f"{PROC_ROOT}/{pid}/exe")
    if target.endswith(DELETED_SUFFIX):
        target = target[: -len(DELETED_SUFFIX)]
    name = os.path.basename(target.rstrip("/"))
    return name or None


def _safe_exe_basename(pid: int) -> Optional[str]:
    try:
        return _exe_basename(pid)
    except OSError:
        # A descendant may exit between the stat read and this lookup.
        return None


def _root_identity(pid: int, expected_starttime: Optional[int]) -> ProcStat:
    """Verify the root is still the same touko-desktop process.

    starttime distinguishes a reused PID from the original root. Rechecking the
    executable basename also catches an exec after launch.
    """
    try:
        stat = _read_stat(pid)
    except PermissionError as exc:
        raise SamplerError(f"cannot read /proc/{pid}/stat: permission denied") from exc
    except OSError as exc:
        raise SamplerError(f"cannot read /proc/{pid}/stat: {exc}") from exc
    if stat is None:
        raise SamplerError(f"root process {pid} no longer exists")
    if expected_starttime is not None and stat.starttime != expected_starttime:
        raise SamplerError(
            f"PID {pid} was reused (starttime {stat.starttime} != {expected_starttime})"
        )
    try:
        executable = _exe_basename(pid)
    except PermissionError as exc:
        raise SamplerError(f"cannot read /proc/{pid}/exe: permission denied") from exc
    except (FileNotFoundError, ProcessLookupError) as exc:
        raise SamplerError(f"root process {pid} exited while checking its executable") from exc
    except OSError as exc:
        raise SamplerError(f"cannot read /proc/{pid}/exe: {exc}") from exc
    if executable is None:
        raise SamplerError(f"cannot resolve the executable of root process {pid}")
    if executable != TARGET_EXECUTABLE:
        raise SamplerError(
            f"PID {pid} runs {executable!r}, not the required {TARGET_EXECUTABLE!r}"
        )
    return stat


def _snapshot_tree(root_pid: int) -> list[ProcStat]:
    """Take one point-in-time table of all processes, then keep root's tree.

    A child exiting between directory enumeration and reading its stat is
    normal and is skipped. Permission failures on unrelated /proc entries do
    not abort the run; a missing root is always fatal.
    """
    table: dict[int, ProcStat] = {}
    try:
        names = os.listdir(PROC_ROOT)
    except OSError as exc:
        raise SamplerError(f"cannot enumerate {PROC_ROOT}: {exc}") from exc
    for name in names:
        if not name.isdigit():
            continue
        pid = int(name)
        try:
            stat = _read_stat(pid)
        except OSError:
            # Another user's process under hidepid, or a process that exited.
            continue
        if stat is not None:
            table[pid] = stat

    if root_pid not in table:
        raise SamplerError(f"root process {root_pid} exited while sampling")

    children: dict[int, list[int]] = defaultdict(list)
    for stat in table.values():
        if stat.pid != root_pid:
            children[stat.ppid].append(stat.pid)

    ordered: list[ProcStat] = []
    seen: set[int] = set()
    stack = [root_pid]
    while stack:
        pid = stack.pop()
        if pid in seen:
            continue
        seen.add(pid)
        stat = table.get(pid)
        if stat is None:
            continue
        ordered.append(stat)
        stack.extend(children.get(pid, ()))
    return ordered


def _classify(stat: ProcStat, root_pid: int) -> dict:
    name = _safe_exe_basename(stat.pid) or "unknown"
    if stat.pid == root_pid:
        role = "touko-desktop"
    elif name == "WebKitWebProcess":
        role = "WebKitWebProcess"
    elif name == "WebKitNetworkProcess":
        role = "WebKitNetworkProcess"
    else:
        role = "other"
    return {
        "pid": stat.pid,
        "ppid": stat.ppid,
        "name": name,
        "role": role,
        "rss_kib": stat.rss_kib,
    }


def _sample(root_pid: int, root_starttime: int) -> Sample:
    timestamp = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
    _root_identity(root_pid, root_starttime)
    stats = _snapshot_tree(root_pid)

    processes = [_classify(stat, root_pid) for stat in stats]
    processes.sort(key=lambda process: (process["pid"] != root_pid, process["pid"]))

    aggregates = {key: {"count": 0, "rss_kib": 0} for key in CATEGORY_KEYS}
    aggregates["tree"] = {"count": 0, "rss_kib": 0}
    for process in processes:
        for key in (process["role"], "tree"):
            aggregates[key]["count"] += 1
            aggregates[key]["rss_kib"] += process["rss_kib"]

    # A root that exits or is reused during the walk must not yield a partial row.
    _root_identity(root_pid, root_starttime)
    return Sample(timestamp, processes, aggregates, aggregates["tree"]["rss_kib"])


def _append_record(path: str, record: dict) -> None:
    line = json.dumps(record, separators=(",", ":"), ensure_ascii=False) + "\n"
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    encoded = line.encode("utf-8")
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        offset = 0
        while offset < len(encoded):
            written = os.write(descriptor, encoded[offset:])
            if written <= 0:
                raise OSError("short write while appending the JSONL record")
            offset += written
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _print_summary(record: dict, output: str) -> None:
    aggregates = record["aggregates"]
    root_line = (
        f"phase={record['phase']} root={record['root_pid']} "
        f"starttime={record['root_starttime']} "
        f"tree={aggregates['tree']['count']} proc(s) "
        f"{aggregates['tree']['rss_kib']} KiB RSS"
    )
    print(root_line)
    for key in CATEGORY_KEYS:
        values = aggregates[key]
        label = key
        if values["count"] == 0 and key in WEBKIT_KEYS:
            label += " (absent)"
        print(f"  {label:<26} {values['count']:>3} proc(s) {values['rss_kib']:>10} KiB")
    print(
        f"  recorded {record['timestamp']} -> {output} "
        f"(sample {record['selected_sample']}/{record['samples']}, "
        f"highest tree RSS)"
    )
    if record["phase"] != "startup":
        absent = [key for key in WEBKIT_KEYS if aggregates[key]["count"] == 0]
        if absent:
            print(
                "desktop-memory: note: "
                + ", ".join(absent)
                + " absent in this snapshot; the phase label is the caller's "
                "observation and does not prove browser readiness.",
                file=sys.stderr,
            )


def _positive_pid(value: str) -> int:
    try:
        pid = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("PID must be an integer") from exc
    if pid <= 0:
        raise argparse.ArgumentTypeError("PID must be positive")
    return pid


def _positive_int(value: str) -> int:
    try:
        number = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("samples must be an integer") from exc
    if number < 1:
        raise argparse.ArgumentTypeError("samples must be at least 1")
    return number


def _nonnegative_seconds(value: str) -> float:
    try:
        number = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("interval must be a number") from exc
    if not math.isfinite(number) or number < 0:
        raise argparse.ArgumentTypeError("interval must be a finite, non-negative number")
    return number


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Snapshot Linux RSS for a touko-desktop process and every process "
            "in its app-owned tree, appending one JSONL record per invocation."
        ),
        epilog=(
            "The target is accepted only when /proc/PID/exe has basename "
            "'touko-desktop'. The root starttime is checked before and after "
            "every sample, so an exited or reused PID fails non-zero instead of "
            "being reported as zero memory. Descendants are the actual PPID "
            "tree only; unrelated WebKit processes are ignored. An absent "
            "WebKitWebProcess or WebKitNetworkProcess is reported with count 0, "
            "not as proof that the browser is ready.\n\n"
            "Phases are caller-supplied labels: this script cannot observe page "
            "readiness. Use 'loaded' only after the native MyGo window has "
            "actually rendered the gallery, 'browsed' only after real native GUI "
            "browsing such as scrolling in that window, and 'committed' after the "
            "commit completed. Do not use these labels merely because the HTTP "
            "server or CLI is alive.\n\n"
            "RSS values come from /proc/PID/stat: they are point-in-time resident "
            "set sizes, not peak memory, PSS, or USS, and shared pages are counted "
            "in each process. With --samples N, N snapshots are taken and the one "
            "with the highest tree RSS is recorded; tree_rss_kib_observed lists "
            "every total for reference. This does not establish a true peak.\n\n"
            "Linux only. No application endpoint, window, state directory, or "
            "service is touched."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--pid", required=True, type=_positive_pid, metavar="PID",
                        help="root touko-desktop PID")
    parser.add_argument("--phase", required=True, choices=PHASES,
                        help="caller-observed phase label")
    parser.add_argument("--output", required=True, metavar="PATH",
                        help="JSONL file to append to")
    parser.add_argument("--samples", type=_positive_int, default=5, metavar="N",
                        help="snapshots to take before recording (default: 5)")
    parser.add_argument("--interval", type=_nonnegative_seconds, default=0.2,
                        metavar="SECONDS",
                        help="delay between snapshots (default: 0.2)")
    return parser


def main(argv: Optional[list[str]] = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if not sys.platform.startswith("linux"):
        parser.error("this profiler reads Linux /proc and is Linux-only")

    try:
        root_stat = _root_identity(args.pid, None)
        samples: list[Sample] = []
        for index in range(args.samples):
            samples.append(_sample(args.pid, root_stat.starttime))
            if index + 1 < args.samples and args.interval > 0:
                time.sleep(args.interval)

        selected_index = max(
            range(len(samples)), key=lambda index: samples[index].tree_rss_kib
        )
        selected = samples[selected_index]
        record = {
            "timestamp": selected.timestamp,
            "phase": args.phase,
            "root_pid": args.pid,
            "root_starttime": root_stat.starttime,
            "root_executable": TARGET_EXECUTABLE,
            "samples": len(samples),
            "interval_seconds": args.interval,
            "selected_sample": selected_index + 1,
            "selection": "max_tree_rss",
            "tree_rss_kib_observed": [sample.tree_rss_kib for sample in samples],
            "processes": selected.processes,
            "aggregates": selected.aggregates,
        }
        _append_record(args.output, record)
    except SamplerError as exc:
        print(f"desktop-memory: {exc}", file=sys.stderr)
        return 1
    except OSError as exc:
        print(f"desktop-memory: cannot append to {args.output}: {exc}", file=sys.stderr)
        return 1

    _print_summary(record, args.output)
    return 0


if __name__ == "__main__":
    sys.exit(main())
