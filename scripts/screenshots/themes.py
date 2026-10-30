"""themes.py OUT NAME=PNG...: the screenshots of several themes in a 2-wide
grid at half size, each with its name below it."""
import sys

from PIL import Image, ImageDraw, ImageFont

out, shots = sys.argv[1], [a.split("=", 1) for a in sys.argv[2:]]
imgs = [(name, Image.open(path).convert("RGB")) for name, path in shots]
w, h = imgs[0][1].width // 2, imgs[0][1].height // 2
gap, label = 12, 34
cols = 2
rows = (len(imgs) + cols - 1) // cols
grid = Image.new("RGB", (cols * w + (cols - 1) * gap, rows * (h + label) + (rows - 1) * gap), (255, 255, 255))
font = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", 20)
d = ImageDraw.Draw(grid)
for i, (name, img) in enumerate(imgs):
    x, y = (i % cols) * (w + gap), (i // cols) * (h + label + gap)
    grid.paste(img.resize((w, h), Image.LANCZOS), (x, y))
    tw = d.textlength(name, font=font)
    d.text((x + (w - tw) / 2, y + h + 6), name, fill=(40, 40, 40), font=font)
grid.save(out)
