#!/usr/bin/env python3
"""生成 CLink 应用图标（512x512 渐变 + 像素风 C 标记）→ build/appicon.png"""
import zlib, struct, os, sys

W = H = 512

def chunk(tag, data):
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xffffffff)

def lerp(a, b, t):
    return tuple(int(a[i] + (b[i] - a[i]) * t) for i in range(3))

TOP = (52, 211, 153)    # 绿
BOT = (16, 100, 90)     # 深绿
DARK = (18, 24, 38)

# 生成 RGBA 像素
rows = []
for y in range(H):
    t = y / (H - 1)
    base = lerp(TOP, BOT, t)
    row = bytearray()
    for x in range(W):
        r, g, b = base
        # 圆角遮罩
        cx, cy = abs(x - W / 2), abs(y - H / 2)
        half = W / 2 - 40
        if cx > half or cy > half:
            corner = max(cx - half, cy - half)
            import math
            d = math.hypot(max(cx - half, 0), max(cy - half, 0))
            if d > 60:
                row += bytes([0, 0, 0, 0])
                continue
        # 像素风 "C"：粗环，右侧开口
        import math
        dx, dy = x - W / 2, y - H / 2
        dist = math.hypot(dx, dy)
        if 120 <= dist <= 200 and not (dx > 60 and -80 < dy < 80):
            row += bytes([DARK[0], DARK[1], DARK[2], 255])
            continue
        # 内部小方块装饰（矿）
        if 230 <= x <= 280 and 230 <= y <= 280:
            row += bytes([251, 191, 36, 255])
            continue
        row += bytes([r, g, b, 255])
    rows.append(bytes(row))

raw = b"".join(b"\x00" + r for r in rows)

png = (b"\x89PNG\r\n\x1a\n"
       + chunk(b"IHDR", struct.pack(">IIBBBBB", W, H, 8, 6, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw, 9))
       + chunk(b"IEND", b""))

out = os.path.join(os.path.dirname(__file__), "..", "build", "appicon.png")
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, "wb") as f:
    f.write(png)
print("written:", os.path.abspath(out), len(png), "bytes")
