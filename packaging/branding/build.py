#!/usr/bin/env python3
"""Rasterise the brand artwork.

badge.svg, mark.svg and mark-white.svg are drawn by hand from the Gniza
mark's own outline, so there is nothing to generate: the shape is a path,
not letters set in a font, and it is the same on a machine that has none of
our fonts. What is left is turning them into the PNGs cPanel needs, because
cPanel's SVG installer rewrites geometry attributes in custom artwork and
draws the account tile from a bitmap instead.

Needs rsvg-convert. It is not a build dependency: the output is committed,
and this is how it was made.

    python3 packaging/branding/build.py
"""

import pathlib
import subprocess

HERE = pathlib.Path(__file__).resolve().parent

BADGE_SIZES = (16, 32, 48, 64, 128, 256, 512)
MARK_SIZES = (256, 512)


def main():
    png = HERE / "png"
    png.mkdir(exist_ok=True)

    for size in BADGE_SIZES:
        # Square: the badge is a square, so height follows width.
        subprocess.run([
            "rsvg-convert", "-w", str(size), "-h", str(size),
            str(HERE / "badge.svg"), "-o", str(png / f"badge-{size}.png"),
        ], check=True)

    for size in MARK_SIZES:
        # Taller than wide, and cropped to the ink, so only the height is
        # given and the width follows the artwork.
        subprocess.run([
            "rsvg-convert", "-h", str(size),
            str(HERE / "mark.svg"), "-o", str(png / f"mark-{size}.png"),
        ], check=True)

    print("wrote", len(BADGE_SIZES) + len(MARK_SIZES), "PNGs to", png)


if __name__ == "__main__":
    main()
