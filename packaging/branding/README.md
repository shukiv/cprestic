# Brand assets

The mark is **cP:R** — cPanel's two letters, then what does the work. Full
name in prose is **cP:Restic**; `cprest` stays the name of the binaries, the
paths and the Go module.

| File | What it is |
|---|---|
| `cpr-badge.svg` | the badge: white letters on the orange rounded square, 300×300 |
| `cpr-mark.svg` | the letters alone, cPanel orange, transparent, trimmed to the ink |
| `cpr-mark-white.svg` | the same in white, for a dark or coloured ground |
| `png/cpr-badge-{16,32,48,64,128,256,512}.png` | the badge, rasterised |
| `png/cpr-mark-{256,512}.png` | the letters, rasterised, transparent |
| `cprestic-logo.svg` | the wordmark, *cP:Restic* in full |

`cpr-badge.svg` is the plugin's icon: WHM draws it in the sidebar from
`addon_plugins/cprest.svg`, and cPanel draws the account tile from the 48px
PNG, because cPanel's SVG installer rewrites geometry attributes in custom
artwork.

## Colours

| | Hex | Where |
|---|---|---|
| Badge ground | `#E35E30` | the square, matching `.cpr-brand-mark` in the interface |
| cPanel orange | `#CF470C` | the letters `cP` on a light ground |
| Ink | `#141A22` | the rest of the wordmark |

## Why the letters are paths

`cprestic-logo.svg` sets `<text>` and pins it with `textLength`, which is what
you do when the artwork has to hold its shape on a machine that has none of
your fonts. It holds; it is not identical — the fallback face still decides
the letterforms.

The `cpr-*` files are outlines of Fira Sans Bold, the face the interface
already ships, so the badge is the same shape everywhere and a PNG made here
matches the SVG a browser draws. Fira Sans is under the SIL Open Font License
([OFL.txt](../../internal/webui/fonts/OFL.txt)), which covers this.

## Rebuilding them

```bash
python3 packaging/branding/build.py     # needs fonttools + brotli, rsvg-convert
```

The output is committed; the script is how it was made. Change the tracking,
the radius or the ink width there rather than editing the paths by hand.
