#!/usr/bin/env python3
"""Build the AD_EXPLORER prototype.

Markup and behaviour live in one place; the style layer supplies the
token block and overrides. Output is a single self-contained HTML file
(including locally vendored fonts) so it can be opened from disk or
handed to someone else unchanged.

    python3 prototype/src/gen.py
"""

import pathlib
import sys

SRC = pathlib.Path(__file__).resolve().parent
OUT = SRC.parent / "index.html"

VARIANT = "win32-classic"
TITLE = "AD_EXPLORER — Win32 Classic prototype"


def main() -> int:
    template = (SRC / "template.html").read_text(encoding="utf-8")
    body = (SRC / "body.html").read_text(encoding="utf-8").strip()
    js = (SRC / "app.js").read_text(encoding="utf-8").strip()
    faces = (SRC / "styles" / "fonts.css").read_text(encoding="utf-8")
    base = (SRC / "styles" / "base.css").read_text(encoding="utf-8")
    variant = (SRC / "styles" / f"{VARIANT}.css").read_text(encoding="utf-8")

    html = (
        template
        .replace("{{VARIANT}}", VARIANT)
        .replace("{{TITLE}}", TITLE)
        .replace("{{FONTS}}", "")
        .replace("{{CSS}}", faces + "\n\n" + base + "\n\n" + variant)
        .replace("{{BODY}}", body)
        .replace("{{JS}}", js)
    )
    if "{{" in html:
        print("error: unresolved placeholder", file=sys.stderr)
        return 1

    OUT.write_text(html, encoding="utf-8")
    print(f"wrote {OUT.relative_to(SRC.parent.parent)}  ({OUT.stat().st_size // 1024} KiB)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
