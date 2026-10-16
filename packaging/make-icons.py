#!/usr/bin/env python3
"""Derive the TOUKO app icon from the author's logo artwork.

The logo is a 1254x1254 illustration with the wordmark along the bottom, so the
whole file is a poor icon: at panel sizes the figure shrinks and the text turns
to noise. This script crops the square around the head and the photo card, then
resamples that square into the size ladder a Linux desktop expects.

Run from the repository root, with Pillow installed:

    python3 packaging/make-icons.py

The source logo is the author's own file and stays out of the repository; only
the derived PNGs written to internal/desktop/assets/ are committed.
"""

from pathlib import Path

from PIL import Image

SOURCE = Path("logo/TOUKO.png")
OUT = Path("internal/desktop/assets")

# Square around the character's head and the photo card she holds, chosen from
# the 1254x1254 original: it excludes the wordmark and keeps the face legible
# down to 16 px. Change both numbers together to keep the crop square.
CROP = (220, 0, 980, 760)
SIZES = (16, 24, 32, 48, 64, 128, 256)


def main() -> None:
    logo = Image.open(SOURCE).convert("RGBA")
    icon = logo.crop(CROP)
    if icon.width != icon.height:
        raise SystemExit(f"crop is not square: {icon.size}")
    OUT.mkdir(parents=True, exist_ok=True)
    for size in SIZES:
        icon.resize((size, size), Image.LANCZOS).save(OUT / f"touko-{size}.png")
        print(f"{OUT / f'touko-{size}.png'} <- {CROP}")


if __name__ == "__main__":
    main()
