#!/usr/bin/env python3
"""Render the README walkthrough from verified scan output and UI captures (Pillow)."""
from pathlib import Path
import argparse
import re
from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / ".github/assets/demo"
WIDTH, HEIGHT = 1200, 860
INK, MUTED, ACCENT = "#162430", "#536371", "#454fbd"
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--font-dir", type=Path, default=Path("/System/Library/Fonts"),
                    help="macOS font directory, or a directory containing DejaVu fonts")
parser.add_argument("--preview-dir", type=Path, help="also save individual scene PNGs")
args = parser.parse_args()


def font(size, bold=False, mono=False):
    path = args.font_dir / ("Menlo.ttc" if mono else f"Supplemental/Arial{' Bold' if bold else ''}.ttf")
    if not path.exists():
        path = args.font_dir / ("DejaVuSansMono.ttf" if mono else f"DejaVuSans{'-Bold' if bold else ''}.ttf")
    return ImageFont.truetype(str(path), size)


def text(draw, xy, value, size=24, color=INK, bold=False, mono=False):
    draw.text(xy, value, font=font(size, bold, mono), fill=color)


def scene(step, title, subtitle, source):
    im = Image.new("RGB", (WIDTH, HEIGHT), "#f5f6f8")
    d = ImageDraw.Draw(im)
    text(d, (38, 25), "TETRIZ HARNESS", 18, ACCENT, True)
    text(d, (1015, 25), f"{step} / 6", 18, MUTED)
    text(d, (38, 67), title, 39, bold=True)
    text(d, (38, 121), subtitle, 23, MUTED)
    d.line((38, 793, 1162, 793), fill="#dce1e6", width=1)
    text(d, (38, 814), source, 17, MUTED)
    for i in range(6):
        d.rounded_rectangle((970+i*32, 817, 992+i*32, 823), radius=3,
                            fill=ACCENT if i < step else "#dce1e6")
    return im, d


def panel(d, box):
    d.rounded_rectangle(box, radius=14, fill="white", outline="#dce1e6", width=2)


def capture(im, name, crop, box):
    shot = Image.open(ASSETS / name).convert("RGB").crop(crop)
    shot.thumbnail((box[2]-box[0], box[3]-box[1]), Image.Resampling.LANCZOS)
    im.paste(shot, (box[0] + (box[2]-box[0]-shot.width)//2, box[1]))


before = (ASSETS / "scan-before.txt").read_text()
after = (ASSETS / "scan-after.txt").read_text()
finding = "Rule pattern matches no files in the repo"
if finding not in before or finding in after:
    raise SystemExit("Scan evidence does not support the demonstrated fix")
counts = [int(re.search(r"(\d+) DETECTED finding", s)[1]) for s in (before, after)]
if counts != [6, 5]:
    raise SystemExit(f"Unexpected scan counts: {counts}")

frames = []
im, d = scene(1, "Find problems in your agent's instructions",
              "Scan the rules, skills and settings already in your repository.",
              "Actual scan results · bundled sample repository · no model call")
panel(d, (38, 187, 1162, 746))
text(d, (68, 214), "recommendations scan --harness-dir <your-repo> ...", 22, mono=True)
text(d, (68, 273), "9 artifacts scanned", 30, bold=True)
text(d, (700, 273), "6 problems found", 30, ACCENT, True)
rows = ["Permissions bypass all prompts", finding, "Over-broad permission grants (2)",
        "The same rule ID appears twice", "Instructions are unusually dense"]
for i, row in enumerate(rows):
    y = 342 + i*70
    if i == 1:
        d.rounded_rectangle((57, y-10, 1142, y+47), radius=8, fill="#eef0ff")
    text(d, (76, y), f"{i+1:02}", 22, ACCENT, True)
    text(d, (130, y), row, 25, bold=i == 1)
frames.append((im, 4800))

im, d = scene(2, "This rule never reaches your Go files",
              "The scan identifies a path pattern that matches nothing.",
              "Finding from the scanner · explanation and proposed fix are human reviewed")
panel(d, (38, 187, 1162, 746))
text(d, (70, 220), ".claude/rules/a.md", 24, MUTED, mono=True)
text(d, (70, 287), "paths: src/legacy/**/*.go", 35, "#a83d35", mono=True)
text(d, (70, 368), "But the repository contains:", 25, bold=True)
text(d, (70, 416), "src/main.go", 31, mono=True)
text(d, (70, 510), "Result: the error-handling rule is never selected", 29, bold=True)
text(d, (70, 556), "for this source file.", 29, bold=True)
text(d, (70, 651), "Suggested fix: target src/**/*.go", 29, ACCENT, True)
frames.append((im, 5200))

im, d = scene(3, "Review the fix. Scan again.",
              "Keep the instruction. Correct the files it applies to.",
              "Actual second scan · one finding resolved · five other findings remain")
panel(d, (38, 187, 1162, 746))
text(d, (70, 223), "BEFORE", 18, MUTED, True)
text(d, (70, 263), "- paths: src/legacy/**/*.go", 31, "#a83d35", mono=True)
text(d, (70, 335), "AFTER", 18, MUTED, True)
text(d, (70, 375), "+ paths: src/**/*.go", 31, "#247351", mono=True)
text(d, (70, 472), "Always wrap errors with context.", 28, bold=True)
text(d, (70, 564), "6 findings  →  5 findings", 40, ACCENT, True)
text(d, (70, 638), '“Rule pattern matches no files” is gone.', 28)
frames.append((im, 4800))

im, d = scene(4, "Publish the approved rule",
              "Copy the reviewed content into the control plane; publishing is explicit.",
              "Actual admin UI · mock data · separate distribution example")
capture(im, "composer.png", (225, 18, 1370, 667), (38, 180, 1162, 780))
frames.append((im, 5200))

im, d = scene(5, "Try it with one team first",
              "Choose a repository and the people who receive the rule.",
              "Actual admin UI · mock data · Platform team selected; other teams untargeted")
capture(im, "composer.png", (234, 678, 934, 1249), (38, 185, 756, 775))
text(d, (782, 260), "Repository", 26, bold=True)
text(d, (782, 307), "example-org/", 23, mono=True)
text(d, (782, 341), "example-repo", 23, mono=True)
text(d, (782, 439), "Audience", 26, bold=True)
text(d, (782, 485), "Platform team", 28, ACCENT, True)
text(d, (782, 579), "You decide when", 25)
text(d, (782, 617), "to expand the rollout.", 25)
frames.append((im, 4800))

im, d = scene(6, "See who is still waiting for the rule",
              "Per-person delivery status makes the rollout visible.",
              "Actual admin UI · mock data · pending is not installed; agents must sync")
capture(im, "published.png", (231, 123, 1365, 811), (38, 180, 1020, 775))
frames.append((im, 5600))

out = ROOT / ".github/assets/demo.gif"
frames[0][0].save(out, save_all=True, append_images=[f for f, _ in frames[1:]],
                  duration=[t for _, t in frames], loop=0, optimize=True, disposal=2)
if args.preview_dir:
    args.preview_dir.mkdir(parents=True, exist_ok=True)
    for i, (frame, _) in enumerate(frames, 1):
        frame.save(args.preview_dir / f"scene-{i}.png")
print(f"Rendered {out}: {len(frames)} scenes, {sum(t for _, t in frames)/1000:.1f}s")
