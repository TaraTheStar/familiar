#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""
Parametric pixel-art generator for "familiar" avatar faces (PROTOCOL_V2 / WS4 F3).

The StackChan avatar is composed by the firmware from sprite layers driven by the
existing animation modifiers (see stackchan/avatar/skins/familiar/):

    cat_face.png         costume: head + ears + whiskers (static, behind)
    cat_eye_open.png     eye, open  (shown when the eye "weight" is high)
    cat_eye_closed.png   eye, blink (shown when weight is low -> BlinkModifier)
    cat_mouth_closed.png mouth, resting
    cat_mouth_open.png   mouth, talking (SpeakingModifier toggles to this)

This is the PLACEHOLDER cat: a clean, reproducible starting point. To add a
familiar (bat, toad, ...) draw the same five sprites named "<familiar>_*.png",
drop them in ../assets_bin/, register the name in familiar_registry.cpp, and
reflash. Hand-drawn or externally AI-generated art using the same names/sizes is
a drop-in replacement for these.

Run:  python3 gen_familiars.py        (writes PNGs into ../assets_bin/)
Deps: Pillow.

The generated PNGs are committed, so the firmware build does NOT depend on this
script; it exists so the art is regeneratable and tweakable.
"""

import os
from PIL import Image, ImageDraw

OUT = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "assets_bin"))

# Canvas: full face is 320x240 (the panel). Eye/mouth sprites are small tiles
# the firmware positions over the costume via LV_ALIGN_CENTER + offset.
FACE_W, FACE_H = 320, 240
EYE = 64          # eye tile (square)
MOUTH_W, MOUTH_H = 96, 56

# Cat palette.
FUR = (120, 124, 138, 255)        # slate-grey fur
FUR_DK = (92, 96, 110, 255)       # ear/edge shadow
INNER_EAR = (208, 150, 170, 255)  # pink inner ear
EYE_IRIS = (168, 224, 120, 255)   # green-yellow
EYE_GLINT = (240, 255, 220, 255)
PUPIL = (24, 26, 32, 255)
NOSE = (214, 138, 158, 255)
MOUTHLINE = (40, 42, 50, 255)
MOUTH_IN = (54, 30, 40, 255)
TONGUE = (224, 132, 150, 255)
WHISKER = (228, 230, 236, 255)
CLEAR = (0, 0, 0, 0)


def _img(w, h):
    return Image.new("RGBA", (w, h), CLEAR)


def gen_face(path):
    """Cat head: fur dome + two triangular ears + whiskers. Transparent bg so
    the black avatar panel shows through; eye/mouth sprites layer on top."""
    im = _img(FACE_W, FACE_H)
    d = ImageDraw.Draw(im)
    cx = FACE_W // 2

    # Ears (drawn first, behind the head dome).
    for sign in (-1, 1):
        bx = cx + sign * 92
        outer = [(bx - 40, 96), (bx + 40, 96), (bx + sign * 6, 8)]
        d.polygon(outer, fill=FUR_DK)
        inner = [(bx - 22, 92), (bx + 22, 92), (bx + sign * 4, 38)]
        d.polygon(inner, fill=INNER_EAR)

    # Head dome: a big rounded rectangle covering most of the face.
    d.rounded_rectangle([cx - 132, 70, cx + 132, 300], radius=120, fill=FUR)

    # Cheek tufts.
    for sign in (-1, 1):
        d.polygon(
            [(cx + sign * 120, 150), (cx + sign * 156, 138), (cx + sign * 150, 176)],
            fill=FUR,
        )

    # Whiskers (both sides of the muzzle).
    for sign in (-1, 1):
        for dy in (-10, 4, 18):
            x0 = cx + sign * 26
            y0 = 168 + dy
            x1 = cx + sign * 150
            y1 = 168 + int(dy * 1.7)
            d.line([(x0, y0), (x1, y1)], fill=WHISKER, width=3)

    im.save(path)
    return im


def gen_eye_open(path):
    im = _img(EYE, EYE)
    d = ImageDraw.Draw(im)
    c = EYE // 2
    # Almond iris.
    d.ellipse([8, 12, EYE - 8, EYE - 12], fill=EYE_IRIS)
    # Vertical cat slit pupil.
    d.ellipse([c - 6, 8, c + 6, EYE - 8], fill=PUPIL)
    # Glint.
    d.ellipse([c + 4, 18, c + 14, 28], fill=EYE_GLINT)
    im.save(path)


def gen_eye_closed(path):
    im = _img(EYE, EYE)
    d = ImageDraw.Draw(im)
    c = EYE // 2
    # A downward arc — the classic "happy/blink" closed cat eye.
    d.arc([10, c - 14, EYE - 10, c + 22], start=200, end=340, fill=PUPIL, width=5)
    im.save(path)


def _nose(d, cx, top):
    d.polygon([(cx - 11, top), (cx + 11, top), (cx, top + 12)], fill=NOSE)


def gen_mouth_closed(path):
    im = _img(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _nose(d, cx, 6)
    # "ω" resting mouth: two small downward curves under the nose.
    d.arc([cx - 22, 16, cx, 40], start=20, end=160, fill=MOUTHLINE, width=4)
    d.arc([cx, 16, cx + 22, 40], start=20, end=160, fill=MOUTHLINE, width=4)
    im.save(path)


def gen_mouth_open(path):
    im = _img(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _nose(d, cx, 2)
    # Open oval with a tongue.
    d.ellipse([cx - 20, 18, cx + 20, MOUTH_H - 4], fill=MOUTH_IN)
    d.ellipse([cx - 12, 34, cx + 12, MOUTH_H - 6], fill=TONGUE)
    im.save(path)


def main():
    os.makedirs(OUT, exist_ok=True)
    gen_face(os.path.join(OUT, "cat_face.png"))
    gen_eye_open(os.path.join(OUT, "cat_eye_open.png"))
    gen_eye_closed(os.path.join(OUT, "cat_eye_closed.png"))
    gen_mouth_closed(os.path.join(OUT, "cat_mouth_closed.png"))
    gen_mouth_open(os.path.join(OUT, "cat_mouth_open.png"))
    print("wrote cat_* sprites to", OUT)
    for f in sorted(os.listdir(OUT)):
        if f.startswith("cat_"):
            print("  ", f, os.path.getsize(os.path.join(OUT, f)), "bytes")


if __name__ == "__main__":
    main()
