#!/usr/bin/env python3
"""Derive every TOUKO icon from the preferred square logo artwork.

The preferred logo is already a 1254x1254 illustration. It is resized as a
whole so the desktop icon, favicon and tab icon all use the same drawing; the
old head-only crop of the alternate artwork is intentionally retired.

Run from the repository root, with Pillow installed:

    python3 packaging/make-icons.py

The source logo is the author's own file and stays out of the repository; only
the derived PNGs and the embedded favicon data are committed.
"""

import re
from base64 import b64encode
from io import BytesIO
from pathlib import Path

from PIL import Image

SOURCE = Path("logo/touko-logo.png")
OUT = Path("internal/desktop/assets")
FAVICON = Path("internal/webview/static/favicon.png")
FAVICON_SVG = Path("internal/webview/static/favicon.svg")
SIZES = (16, 24, 32, 48, 64, 128, 256)


def png_bytes(image: Image.Image) -> bytes:
    buffer = BytesIO()
    image.save(buffer, "PNG")
    return buffer.getvalue()


def main() -> None:
    logo = Image.open(SOURCE).convert("RGBA")
    if logo.width != logo.height:
        raise SystemExit(f"logo is not square: {logo.size}")
    OUT.mkdir(parents=True, exist_ok=True)
    for size in SIZES:
        icon = logo.resize((size, size), Image.LANCZOS)
        path = OUT / f"touko-{size}.png"
        icon.save(path)
        print(f"{path} <- {SOURCE} {logo.size}")
    favicon = logo.resize((128, 128), Image.LANCZOS)
    FAVICON.write_bytes(png_bytes(favicon))
    print(f"{FAVICON} <- {SOURCE} {logo.size}")
    encoded = b64encode(png_bytes(favicon)).decode("ascii")
    svg = FAVICON_SVG.read_text()
    count = len(re.findall(r"data:image/png;base64,[^\"]+", svg))
    FAVICON_SVG.write_text(
        re.sub(r"data:image/png;base64,[^\"]+", f"data:image/png;base64,{encoded}", svg)
    )
    print(f"{FAVICON_SVG} <- {count} embedded image(s)")


if __name__ == "__main__":
    main()
