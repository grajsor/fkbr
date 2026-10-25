"""shoot.py OUTDIR STEPS CMD...: drive a TUI in a pty and save PNG screenshots.

STEPS is a file of lines: "keys <name>..." sends keys, "type <text>"
types text, "wait <seconds>",
"shot <name>" saves OUTDIR/<name>.png and prints the screen text.
"""
import fcntl
import os
import pty
import select
import struct
import sys
import termios
import time

import pyte
from PIL import Image, ImageDraw, ImageFont

COLS, ROWS = 120, 34
KEYS = {
    "up": b"\x1b[A", "down": b"\x1b[B", "enter": b"\r", "tab": b"\t",
    "insert": b"\x1b[2~", "esc": b"\x1b", "bs": b"\x7f", "home": b"\x1b[H",
    "f1": b"\x1bOP", "f2": b"\x1bOQ", "f3": b"\x1bOR", "f4": b"\x1bOS", "f5": b"\x1b[15~",
    "f9": b"\x1b[20~", "f11": b"\x1b[23~", "ctrlc": b"\x03", "ctrla": b"\x01", "ctrlo": b"\x0f",
    "ctrle": b"\x05", "ctrlw": b"\x17", "right": b"\x1b[C", "del": b"\x1b[3~", "n": b"n",
}
NAMED = {  # xterm defaults, for 16-colour output
    "black": (0, 0, 0), "red": (205, 0, 0), "green": (0, 205, 0), "brown": (205, 205, 0),
    "blue": (0, 0, 238), "magenta": (205, 0, 205), "cyan": (0, 205, 205), "white": (229, 229, 229),
}
DEF_FG, DEF_BG = (204, 204, 204), (24, 24, 24)

outdir, steps_file, cmd = sys.argv[1], sys.argv[2], sys.argv[3:]
screen = pyte.Screen(COLS, ROWS)
stream = pyte.ByteStream(screen)

pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))


def pump(seconds):
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.05)
        if not r:
            continue
        try:
            data = os.read(fd, 65536)
        except OSError:
            return
        if b"\x1b]11;?" in data:
            os.write(fd, b"\x1b]11;rgb:1818/1818/1818\x1b\\")
        if b"\x1b[6n" in data:
            os.write(fd, b"\x1b[1;1R")
        stream.feed(data)


def color(c, default):
    if c == "default":
        return default
    if c in NAMED:
        return NAMED[c]
    if c.startswith("bright"):
        return tuple(min(255, v + 50) for v in NAMED.get(c[6:], default))
    try:
        return tuple(int(c[i:i + 2], 16) for i in (0, 2, 4))
    except ValueError:
        return default


# SCALE=2 renders at twice the size (36 px font, 2640x1462); in practice
# it looked the same, only bigger, so 1 is the default.
SCALE = int(os.environ.get("SCALE", "1"))
SIZE = 18 * SCALE
font = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf", SIZE)
bold = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf", SIZE)
asc, desc = font.getmetrics()
CW, CH = round(font.getlength("M")), asc + desc

# Box drawing: (left, right, up, down) arm widths, 1 = single, 2 = double.
BOX = {
    "─": (1, 1, 0, 0), "│": (0, 0, 1, 1), "┌": (0, 1, 0, 1), "┐": (1, 0, 0, 1),
    "└": (0, 1, 1, 0), "┘": (1, 0, 1, 0), "═": (2, 2, 0, 0), "║": (0, 0, 2, 2),
    "╔": (0, 2, 0, 2), "╗": (2, 0, 0, 2), "╚": (0, 2, 2, 0), "╝": (2, 0, 2, 0),
}


def box(d, ch, x0, y0, fg):
    left, right, up, down = BOX[ch]
    cx, cy = x0 + CW // 2, y0 + CH // 2
    gap = 3 * SCALE  # distance between the two lines of a double stroke
    w = SCALE  # stroke width

    def hline(xa, xb, n):
        offs = [0] if n == 1 else [-gap // 2 - 1, gap // 2 + 1]
        for o in offs:
            d.line([(xa, cy + o), (xb, cy + o)], fill=fg, width=w)

    def vline(ya, yb, n):
        offs = [0] if n == 1 else [-gap // 2 - 1, gap // 2 + 1]
        for o in offs:
            d.line([(cx + o, ya), (cx + o, yb)], fill=fg, width=w)

    # Corners of double boxes: draw each arm to the matching inner/outer line.
    if left:
        hline(x0, cx + (gap // 2 + 1 if left == 2 and (up or down) else 0), left)
    if right:
        hline(cx - (gap // 2 + 1 if right == 2 and (up or down) else 0), x0 + CW, right)
    if up:
        vline(y0, cy + (gap // 2 + 1 if up == 2 and (left or right) else 0), up)
    if down:
        vline(cy - (gap // 2 + 1 if down == 2 and (left or right) else 0), y0 + CH, down)


def shot(name):
    img = Image.new("RGB", (COLS * CW, ROWS * CH), DEF_BG)
    d = ImageDraw.Draw(img)
    lines = []
    for y in range(ROWS):
        row = screen.buffer[y]
        text = ""
        for x in range(COLS):
            c = row[x]
            fg, bg = color(c.fg, DEF_FG), color(c.bg, DEF_BG)
            if c.reverse:
                fg, bg = bg, fg
            x0, y0 = x * CW, y * CH
            d.rectangle([x0, y0, x0 + CW - 1, y0 + CH - 1], fill=bg)
            ch = c.data or " "
            text += ch
            if ch in BOX:
                box(d, ch, x0, y0, fg)
            elif ch != " ":
                d.text((x0, y0), ch, fill=fg, font=bold if c.bold else font)
        lines.append(text)
    img.save(os.path.join(outdir, name + ".png"))
    print("=== " + name)
    print("\n".join(lines))


pump(2.5)
for line in open(steps_file):
    parts = line.split()
    if not parts or parts[0].startswith("#"):
        continue
    if parts[0] == "keys":
        for k in parts[1:]:
            os.write(fd, KEYS[k])
            pump(0.35)
    elif parts[0] == "type":  # type the rest of the line as text
        os.write(fd, line.split(" ", 1)[1].rstrip("\n").encode())
        pump(0.35)
    elif parts[0] == "wait":
        pump(float(parts[1]))
    elif parts[0] == "shot":
        pump(0.5)
        shot(parts[1])
os.write(fd, KEYS["ctrlc"])
pump(1)
os.waitpid(pid, 0)
