#!/usr/bin/env python3
"""Render the retained benchmark JSON and RSS JSONL as dependency-free SVGs.

Usage:
    python3 scripts/plot-benchmark.py \
      --benchmark "$ROOT/benchmark.json" \
      --rss "0=$ROOT/profiles/0.jsonl" \
      --rss "1000=$ROOT/profiles/1000.jsonl" \
      --out-dir docs/images

The script reads measurements only; it does not run benchmarks. Numbers are
labelled with their actual units and the source files remain the audit trail.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path
from xml.sax.saxutils import escape

WIDTH = 860
HEIGHT = 450
LEFT = 70
RIGHT = 25
TOP = 105
BOTTOM = 55
INK = "#1b2a3d"
MUTED = "#6b6257"
GRID = "#d9d1c2"
COLORS = ("#1b2a3d", "#c2563a", "#3e7c59", "#8a6a3b", "#6d5b8a")
FONT = "font-family='-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif'"


def svg_header(title, subtitle):
    return [
        f"<svg xmlns='http://www.w3.org/2000/svg' width='{WIDTH}' height='{HEIGHT}' "
        f"viewBox='0 0 {WIDTH} {HEIGHT}' role='img' aria-label='{escape(title)}'>",
        f"<title>{escape(title)}</title>",
        f"<desc>{escape(subtitle)}</desc>",
        f"<rect width='{WIDTH}' height='{HEIGHT}' fill='#fbfaf7'/>",
        f"<text x='{LEFT}' y='28' {FONT} font-size='18' font-weight='600' fill='{INK}'>{escape(title)}</text>",
        f"<text x='{LEFT}' y='48' {FONT} font-size='11' fill='{MUTED}'>{escape(subtitle)}</text>",
    ]


def axis(x, y, w, h, ymin, ymax, ticks):
    out = [f"<rect x='{x}' y='{y}' width='{w}' height='{h}' fill='none' stroke='{GRID}'/>"]
    for tick in ticks:
        py = y + h - (math.log10(tick) - math.log10(ymin)) / (math.log10(ymax) - math.log10(ymin)) * h
        out.append(f"<line x1='{x}' y1='{py:.1f}' x2='{x + w}' y2='{py:.1f}' stroke='{GRID}' stroke-dasharray='3 4'/>")
        out.append(f"<text x='{x - 8}' y='{py + 4:.1f}' text-anchor='end' {FONT} font-size='10' fill='{MUTED}'>{tick:g}</text>")
    return out


def panel(title, metrics, measurements, x, y, w, h, ymin, ymax, ticks):
    counts = [int(value) for value in measurements]
    out = [f"<text x='{x}' y='{y - 12}' {FONT} font-size='12' font-weight='600' fill='{INK}'>{escape(title)}</text>"]
    out += axis(x, y, w, h, ymin, ymax, ticks)
    for category in counts:
        px = x + (counts.index(category) + 0.5) / len(counts) * w
        out.append(f"<text x='{px:.1f}' y='{y + h + 18}' text-anchor='middle' {FONT} font-size='11' fill='{MUTED}'>{category}</text>")
    out.append(f"<text x='{x + w / 2}' y='{y + h + 38}' text-anchor='middle' {FONT} font-size='11' fill='{MUTED}'>committed assets</text>")
    for index, metric in enumerate(metrics):
        points = []
        for category in counts:
            measurement = measurements[str(category)]["metrics"].get(metric, {})
            median = measurement.get("median_seconds")
            if median is None:
                continue
            px = x + (counts.index(category) + 0.5) / len(counts) * w
            py = y + h - (math.log10(median * 1000) - math.log10(ymin)) / (math.log10(ymax) - math.log10(ymin)) * h
            points.append((px, py, median * 1000))
        color = COLORS[index % len(COLORS)]
        if len(points) > 1:
            out.append("<polyline fill='none' stroke='%s' stroke-width='2' points='%s'/>" % (
                color, " ".join(f"{px:.1f},{py:.1f}" for px, py, _ in points)))
        for px, py, value in points:
            out.append(f"<circle cx='{px:.1f}' cy='{py:.1f}' r='3.5' fill='{color}'/>")
        if points:
            px, py, value = points[-1]
            out.append(f"<text x='{px + 7:.1f}' y='{py - 7:.1f}' {FONT} font-size='10' fill='{color}'>{value:,.1f} ms</text>")
        lx = x + index * (w / max(len(metrics), 1))
        out.append(f"<rect x='{lx:.1f}' y='{TOP - 40}' width='9' height='9' fill='{color}'/>")
        out.append(f"<text x='{lx + 14:.1f}' y='{TOP - 32}' {FONT} font-size='10' fill='{MUTED}'>{escape(metric)}</text>")
    out.append(f"<text x='{x - 45}' y='{y + h / 2}' transform='rotate(-90 {x - 45} {y + h / 2})' text-anchor='middle' {FONT} font-size='11' fill='{MUTED}'>median milliseconds (log scale)</text>")
    return out


def cli_chart(benchmark):
    measurements = benchmark["measurements"]
    title = "TOUKO local CLI/HTTP baseline"
    subtitle = (
        "Median of %d repetitions; local Git; %s; batch %d. "
        "Lower is better; scales are logarithmic."
        % (benchmark["parameters"]["repeat"], benchmark["environment"]["machine"], benchmark["parameters"]["batch"])
    )
    out = svg_header(title, subtitle)
    width = (WIDTH - LEFT - RIGHT - 55) / 2
    out += panel("Read paths", ("startup_state", "http_state", "image_bytes", "find"), measurements, LEFT, TOP, width, HEIGHT - TOP - BOTTOM, 0.3, 4000, (1, 10, 100, 1000))
    out += panel("Mutating paths", ("batch_add", "reindex", "commit"), measurements, LEFT + width + 55, TOP, width, HEIGHT - TOP - BOTTOM, 1, 4000, (1, 10, 100, 1000))
    out.append("</svg>")
    return "\n".join(out) + "\n"


def memory_chart(series):
    title = "Native desktop RSS by phase"
    subtitle = "Linux whole-tree RSS; max of 5 snapshots. Shared pages are counted per process."
    out = svg_header(title, subtitle)
    chart_x, chart_y = LEFT + 90, TOP
    chart_w, chart_h = WIDTH - chart_x - RIGHT - 55, 275
    max_rss = max(row["aggregates"]["tree"]["rss_kib"] / 1024 for _, row in series)
    limit = math.ceil(max_rss / 100) * 100
    keys = (
        ("touko-desktop", "touko-desktop", COLORS[0]),
        ("WebKitWebProcess", "WebKit renderer", COLORS[1]),
        ("WebKitNetworkProcess", "WebKit network", COLORS[2]),
        ("other", "other", COLORS[3]),
    )
    for tick in range(0, limit + 1, max(100, limit // 5)):
        px = chart_x + tick / limit * chart_w
        out.append(f"<line x1='{px:.1f}' y1='{chart_y}' x2='{px:.1f}' y2='{chart_y + chart_h}' stroke='{GRID}'/>")
        out.append(f"<text x='{px:.1f}' y='{chart_y + chart_h + 18}' text-anchor='middle' {FONT} font-size='10' fill='{MUTED}'>{tick}</text>")
    out.append(f"<text x='{chart_x + chart_w / 2}' y='{chart_y + chart_h + 38}' text-anchor='middle' {FONT} font-size='11' fill='{MUTED}'>MiB RSS</text>")
    bar_h = chart_h / len(series) * 0.62
    for index, (label, row) in enumerate(series):
        py = chart_y + (index + 0.19) * chart_h / len(series)
        out.append(f"<text x='{chart_x - 10}' y='{py + bar_h / 2 + 4:.1f}' text-anchor='end' {FONT} font-size='10' fill='{INK}'>{escape(label)}</text>")
        offset = 0.0
        total_mib = 0.0
        for key, _, color in keys:
            value = row["aggregates"][key]["rss_kib"] / 1024
            if value <= 0:
                continue
            width = value / limit * chart_w
            out.append(f"<rect x='{chart_x + offset:.1f}' y='{py:.1f}' width='{width:.1f}' height='{bar_h:.1f}' fill='{color}'/>")
            offset += width
            total_mib += value
        out.append(f"<text x='{chart_x + offset + 5:.1f}' y='{py + bar_h / 2 + 4:.1f}' {FONT} font-size='10' fill='{MUTED}'>{total_mib:,.0f} MiB</text>")
    for index, (_, label, color) in enumerate(keys):
        lx = LEFT + index * 160
        out.append(f"<rect x='{lx}' y='{HEIGHT - 20}' width='9' height='9' fill='{color}'/>")
        out.append(f"<text x='{lx + 14}' y='{HEIGHT - 12}' {FONT} font-size='10' fill='{MUTED}'>{escape(label)}</text>")
    out.append("</svg>")
    return "\n".join(out) + "\n"


def parse_rss(value):
    label, separator, path = value.partition("=")
    if not separator or not label or not path:
        raise argparse.ArgumentTypeError("expected LABEL=PATH")
    return label, Path(path)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--benchmark", required=True, type=Path)
    parser.add_argument("--rss", action="append", default=[], type=parse_rss, metavar="LABEL=PATH")
    parser.add_argument("--out-dir", required=True, type=Path)
    args = parser.parse_args(argv)
    benchmark = json.loads(args.benchmark.read_text())
    if not benchmark.get("measurements"):
        parser.error("benchmark.json has no measurements; run without --prepare-only")
    args.out_dir.mkdir(parents=True, exist_ok=True)
    (args.out_dir / "benchmark-cli.svg").write_text(cli_chart(benchmark))
    if args.rss:
        series = []
        for label, path in args.rss:
            for line in path.read_text().splitlines():
                if line.strip():
                    row = json.loads(line)
                    series.append((f"{label} {row['phase']}", row))
        (args.out_dir / "benchmark-memory.svg").write_text(memory_chart(series))
    print(f"wrote {args.out_dir / 'benchmark-cli.svg'}")
    if args.rss:
        print(f"wrote {args.out_dir / 'benchmark-memory.svg'}")


if __name__ == "__main__":
    main(sys.argv[1:])
