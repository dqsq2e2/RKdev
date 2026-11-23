#!/usr/bin/env python3
"""Rasterize Noto Sans CJK SC into the small bitmap font used by RKdev Setup."""
import gzip
import math
import struct
import sys
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


font = ImageFont.truetype(sys.argv[1], 18)
chars = {chr(c) for c in range(32, 127)}
chars.update("·–—…：★✓□→←↑↓×＋−")
for first in range(0xA1, 0xF8):
    for second in range(0xA1, 0xFF):
        try:
            chars.add(bytes((first, second)).decode("gb2312"))
        except UnicodeDecodeError:
            pass
chars.update("".join(Path(x).read_text(encoding="utf-8") for x in sys.argv[3:]))
glyphs = bytearray()
for c in sorted({x for x in chars if x.isprintable()}):
    width = max(3, min(22, math.ceil(font.getlength(c)) + 1))
    glyph = Image.new("L", (width, 23))
    ImageDraw.Draw(glyph).text((0, 19), c, font=font, fill=255, anchor="ls")
    glyphs.extend(struct.pack("<IB", ord(c), width))
    glyphs.extend(bytes(85 * ((alpha + 42) // 85) for alpha in glyph.tobytes()))
Path(sys.argv[2]).write_bytes(gzip.compress(glyphs, mtime=0))
