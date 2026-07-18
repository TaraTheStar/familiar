#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""
Parametric art generator for "familiar" avatar faces (PROTOCOL_V2 / WS4 F3).

The StackChan avatar is composed by the firmware from sprite layers driven by the
existing animation modifiers (see stackchan/avatar/skins/familiar/):

    <name>_face.png          costume: head/ears/markings (static, behind)
    <name>_eye_open.png      eye, open  (shown when the eye "weight" is high)
    <name>_eye_closed.png    eye, blink (shown when weight is low -> BlinkModifier)
    <name>_mouth_closed.png  mouth, resting
    <name>_mouth_open.png    mouth, talking (SpeakingModifier toggles to this)

Ships four familiars: cat, bat, toad, fox. Everything is drawn at 4x resolution
and downsampled (LANCZOS) so edges are antialiased on the 320x240 panel, with
layered shading (edge/core/highlight, soft eye sockets, tapered whiskers).

Geometry contract with familiar.cpp (do not drift):
    - face canvas 320x240; the panel behind it is black
    - eye tiles 64x64, centers at panel-center + (+-64, -20), gaze travel +-16px
    - mouth tile 96x56 (nose included), center at panel-center + (0, +44),
      gaze travel +-12px
Head silhouettes must keep fur under the whole eye-travel box; soft "socket"
shading is drawn there so gaze extremes look intentional.

Run:  python3 gen_familiars.py [--out DIR] [--preview FILE.png]
      --out      defaults to ../assets_bin (the committed sprites)
      --preview  also writes a contact sheet compositing each familiar at the
                 firmware offsets in neutral / blink / talking states
Deps: Pillow.

The generated PNGs are committed, so the firmware build does NOT depend on this
script; it exists so the art is regeneratable and tweakable. Hand-drawn art with
the same names/sizes is a drop-in replacement. To add a familiar: add a section
here (or draw the five sprites by hand), then register the name in
familiar_registry.cpp.
"""

import argparse
import math
import os

from PIL import Image, ImageChops, ImageDraw, ImageFilter

# ------------------------------------------------------------------ plumbing

SS = 4  # supersample factor; all drawing happens at SS * logical resolution

FACE_W, FACE_H = 320, 240
EYE = 64
MOUTH_W, MOUTH_H = 96, 56

CLEAR = (0, 0, 0, 0)


def canvas(w, h):
    return Image.new("RGBA", (w * SS, h * SS), CLEAR)


def sbox(x0, y0, x1, y1):
    return [x0 * SS, y0 * SS, x1 * SS, y1 * SS]


def spts(seq):
    return [(x * SS, y * SS) for x, y in seq]


def down(im, w, h):
    return im.resize((w, h), Image.LANCZOS)


def qbez(p0, p1, p2, n=32):
    """Sample a quadratic bezier p0 -> p2 with control p1."""
    out = []
    for i in range(n + 1):
        t = i / n
        u = 1.0 - t
        out.append((u * u * p0[0] + 2 * u * t * p1[0] + t * t * p2[0],
                    u * u * p0[1] + 2 * u * t * p1[1] + t * t * p2[1]))
    return out


def tapered_stroke(draw, p0, ctrl, p1, w0, w_mid, w1, color, n=36):
    """A curved stroke whose width varies (end, middle, end) — brush-like.
    Points/widths in logical px; drawn onto a supersampled ImageDraw."""
    path = qbez((p0[0] * SS, p0[1] * SS), (ctrl[0] * SS, ctrl[1] * SS),
                (p1[0] * SS, p1[1] * SS), n)
    left, right = [], []
    for i, (x, y) in enumerate(path):
        t = i / n
        # smooth width profile through the three keys
        if t < 0.5:
            w = w0 + (w_mid - w0) * math.sin(t * math.pi)
        else:
            w = w1 + (w_mid - w1) * math.sin(t * math.pi)
        w *= SS * 0.5
        if i == 0:
            dx, dy = path[1][0] - x, path[1][1] - y
        elif i == n:
            dx, dy = x - path[i - 1][0], y - path[i - 1][1]
        else:
            dx, dy = path[i + 1][0] - path[i - 1][0], path[i + 1][1] - path[i - 1][1]
        norm = math.hypot(dx, dy) or 1.0
        nx, ny = -dy / norm, dx / norm
        left.append((x + nx * w, y + ny * w))
        right.append((x - nx * w, y - ny * w))
    draw.polygon(left + right[::-1], fill=color)


def erode_alpha(mask, px):
    """Shrink an L-mode mask inward by ~px logical pixels."""
    size = max(3, 2 * round(px * SS / 2) + 1)
    return mask.filter(ImageFilter.MinFilter(size))


def clipped(base, layer, blur=0.0):
    """Composite layer onto base, but only where base already has alpha."""
    if blur:
        layer = layer.filter(ImageFilter.GaussianBlur(blur * SS))
    a = ImageChops.multiply(layer.getchannel("A"), base.getchannel("A"))
    layer.putalpha(a)
    return Image.alpha_composite(base, layer)


def clip_draw(base, fn, blur=0.0):
    """Draw with fn(ImageDraw) on a fresh layer, clip it inside base's alpha."""
    layer = Image.new("RGBA", base.size, CLEAR)
    fn(ImageDraw.Draw(layer))
    return clipped(base, layer, blur)


def colorized(mask, edge, core, shrink=6, blur=7):
    """Fill a silhouette mask with edge color, lighter core inset — cheap 2-tone
    shading that keeps a soft dark rim around the whole shape."""
    im = Image.new("RGBA", mask.size, edge)
    im.putalpha(mask)
    core_mask = erode_alpha(mask, shrink).filter(ImageFilter.GaussianBlur(blur * SS))
    core_layer = Image.new("RGBA", mask.size, core)
    core_layer.putalpha(core_mask)
    return clipped(im, core_layer)


def glow(base, cx, cy, rx, ry, color, blur=10):
    """Soft clipped radial highlight/shadow ellipse (logical coords)."""
    return clip_draw(
        base, lambda d: d.ellipse(sbox(cx - rx, cy - ry, cx + rx, cy + ry), fill=color),
        blur=blur)


def patch(base, cx, cy, rx, ry, color, blur=2.0):
    """A marking (chin/muzzle/belly): a real shape with a softened edge, unlike
    glow() which is diffuse light. Clipped inside the silhouette."""
    return clip_draw(
        base, lambda d: d.ellipse(sbox(cx - rx, cy - ry, cx + rx, cy + ry), fill=color),
        blur=blur)


def eye_sockets(base, color, y=100, rx=42, ry=34):
    """Soft shadow patches under the eye-travel boxes (centers +-64,-20 from
    panel center = (96,100)/(224,100) on the face canvas, +-16px gaze)."""
    for ex in (96, 224):
        base = glow(base, ex, y, rx, ry, color, blur=8)
    return base


# ------------------------------------------------------------------ cat

CAT = {
    "edge": (86, 80, 112, 255),
    "core": (127, 121, 152, 255),
    "hl": (156, 150, 182, 90),
    "socket": (62, 57, 86, 80),
    "ear_in": (226, 158, 180, 255),
    "ear_in_dk": (176, 112, 136, 255),
    "blush": (214, 138, 158, 60),
    "stripe": (70, 65, 94, 110),
    "whisker": (234, 236, 244, 175),
    "muzzle": (152, 147, 178, 255),
    "line": (30, 30, 40, 255),
    "iris_top": (234, 242, 155, 255),
    "iris_bot": (124, 194, 78, 255),
    "nose": (227, 156, 180, 255),
    "nose_dk": (142, 91, 112, 255),
    "mouth_in": (58, 31, 41, 255),
    "tongue": (232, 137, 155, 255),
    "fang": (244, 244, 240, 255),
}


def cat_face():
    P = CAT
    cx = FACE_W // 2

    # Ears behind the head: curved triangles with rounded tips.
    ears = canvas(FACE_W, FACE_H)
    de = ImageDraw.Draw(ears)
    for s in (-1, 1):
        bx = cx + s * 92
        side_a = qbez(*spts([(bx - s * 44, 108), (bx - s * 30, 40), (bx + s * 6, 8)]))
        side_b = qbez(*spts([(bx + s * 6, 8), (bx + s * 34, 44), (bx + s * 46, 100)]))
        de.polygon(side_a + side_b, fill=P["edge"])
    for s in (-1, 1):
        bx = cx + s * 92
        side_a = qbez(*spts([(bx - s * 26, 96), (bx - s * 18, 48), (bx + s * 4, 30)]))
        side_b = qbez(*spts([(bx + s * 4, 30), (bx + s * 22, 52), (bx + s * 30, 92)]))
        ear_in = Image.new("RGBA", ears.size, CLEAR)
        ImageDraw.Draw(ear_in).polygon(side_a + side_b, fill=P["ear_in"])
        ears = Image.alpha_composite(ears, ear_in)
        ears = clip_draw(  # inner-ear depth shadow at the base
            ears, lambda d, b=bx, ss=s: d.polygon(
                spts([(b - ss * 26, 96), (b + ss * 30, 92), (b + ss * 4, 62)]),
                fill=P["ear_in_dk"]), blur=4)

    # Head silhouette: dome + cheek fluff spikes, merged into one mask.
    mask = Image.new("L", ears.size, 0)
    dm = ImageDraw.Draw(mask)
    dm.rounded_rectangle(sbox(cx - 138, 44, cx + 138, 330), radius=115 * SS, fill=255)
    for s in (-1, 1):  # three-point cheek tufts
        dm.polygon(spts([(cx + s * 126, 136), (cx + s * 160, 128), (cx + s * 146, 156),
                         (cx + s * 164, 162), (cx + s * 142, 182), (cx + s * 154, 196),
                         (cx + s * 120, 200)]), fill=255)
    head = colorized(mask, P["edge"], P["core"])
    head = glow(head, cx, 78, 120, 74, P["hl"], blur=14)          # moonlit crown
    head = eye_sockets(head, P["socket"])
    # slightly lighter muzzle so the lower face isn't a featureless wall
    head = patch(head, cx, 180, 52, 34, P["muzzle"], blur=4.0)
    for s in (-1, 1):                                             # cheek blush
        head = glow(head, cx + s * 86, 152, 26, 16, P["blush"], blur=7)

    # Tabby forehead stripes.
    def stripes(d):
        tapered_stroke(d, (cx, 46), (cx, 62), (cx, 84), 1, 7, 1, P["stripe"])
        tapered_stroke(d, (cx - 22, 50), (cx - 25, 62), (cx - 27, 78), 1, 6, 1, P["stripe"])
        tapered_stroke(d, (cx + 22, 50), (cx + 25, 62), (cx + 27, 78), 1, 6, 1, P["stripe"])
    head = clip_draw(head, stripes, blur=1.2)

    im = Image.alpha_composite(ears, head)

    # Whiskers: fine drooping curves, drawn over the head (behind eye/mouth tiles).
    dw = ImageDraw.Draw(im)
    for s in (-1, 1):
        for (y0, y1, bend) in [(-10, -14, -6), (2, 12, 6), (14, 38, 18)]:
            x0 = cx + s * 44
            x1 = cx + s * 148
            tapered_stroke(dw, (x0, 160 + y0), (cx + s * 102, 156 + y0 + bend),
                           (x1, 160 + y1), 2.2, 1.6, 0.5, P["whisker"])
    return im


def _eye_open(iris_top, iris_bot, line, pupil_kind, pupil, glint_big=6, ring=None):
    """Shared open-eye builder: gradient iris, outline, pupil, glints."""
    im = canvas(EYE, EYE)
    cx, cy, r = 32, 33, 24

    # gradient iris disc
    mask = Image.new("L", im.size, 0)
    ImageDraw.Draw(mask).ellipse(sbox(cx - r, cy - r, cx + r, cy + r), fill=255)
    grad = Image.new("RGBA", im.size, CLEAR)
    dg = ImageDraw.Draw(grad)
    for i in range(64):
        t = i / 63
        col = tuple(round(iris_top[c] + (iris_bot[c] - iris_top[c]) * t) for c in range(3)) + (255,)
        dg.rectangle([0, i * EYE * SS // 64, EYE * SS, (i + 1) * EYE * SS // 64], fill=col)
    grad.putalpha(mask)
    im = Image.alpha_composite(im, grad)

    d = ImageDraw.Draw(im)
    if ring:  # optional inner accent ring (bat's amber rim)
        d.ellipse(sbox(cx - r + 3, cy - r + 3, cx + r - 3, cy + r - 3),
                  outline=ring, width=round(2.5 * SS))
    # pupil
    if pupil_kind == "slit":
        d.polygon(qbez(*spts([(cx, cy - 19), (cx - 6.5, cy), (cx, cy + 19)])) +
                  qbez(*spts([(cx, cy + 19), (cx + 6.5, cy), (cx, cy - 19)])), fill=pupil)
    elif pupil_kind == "bar":  # toad's horizontal pupil
        d.rounded_rectangle(sbox(cx - 13, cy - 5, cx + 13, cy + 5), radius=5 * SS, fill=pupil)
    else:  # round
        d.ellipse(sbox(cx - 11, cy - 11, cx + 11, cy + 11), fill=pupil)
    # outline
    d.ellipse(sbox(cx - r, cy - r, cx + r, cy + r), outline=line, width=round(2.4 * SS))
    # glints
    g = glint_big
    d.ellipse(sbox(cx - 10 - g, cy - 9 - g, cx - 10 + g, cy - 9 + g), fill=(255, 255, 255, 235))
    d.ellipse(sbox(cx + 8 - 3, cy + 10 - 3, cx + 8 + 3, cy + 10 + 3), fill=(255, 255, 255, 140))
    return im


def _eye_closed(line, lashes=True):
    """Thick tapered 'closed lid' arc; reads as a real blink at a distance."""
    im = canvas(EYE, EYE)
    d = ImageDraw.Draw(im)
    tapered_stroke(d, (8, 40), (32, 22), (56, 40), 2.5, 6.5, 2.5, line)
    if lashes:
        tapered_stroke(d, (10, 40), (7, 43), (5, 48), 3.0, 2.0, 0.8, line)
        tapered_stroke(d, (54, 40), (57, 43), (59, 48), 3.0, 2.0, 0.8, line)
    return im


def _cat_nose(d, cx, top, P):
    body = qbez(*spts([(cx - 10, top), (cx, top + 2), (cx + 10, top)])) + \
           qbez(*spts([(cx + 10, top), (cx + 7, top + 9), (cx, top + 12)])) + \
           qbez(*spts([(cx, top + 12), (cx - 7, top + 9), (cx - 10, top)]))
    d.polygon(body, fill=P["nose"])
    d.line(spts([(cx, top + 11), (cx, top + 17)]), fill=P["nose_dk"], width=round(1.6 * SS))
    d.ellipse(sbox(cx - 6, top + 2, cx - 2, top + 5), fill=(255, 255, 255, 90))


def cat_mouth_closed():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _cat_nose(d, cx, 5, CAT)
    tapered_stroke(d, (cx - 20, 24), (cx - 10, 34), (cx, 25), 1.2, 3.4, 2.2, CAT["line"])
    tapered_stroke(d, (cx, 25), (cx + 10, 34), (cx + 20, 24), 2.2, 3.4, 1.2, CAT["line"])
    return im


def cat_mouth_open():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _cat_nose(d, cx, 2, CAT)
    # open mouth: rounded blob, tongue, two fang tips hanging from the top lip
    d.ellipse(sbox(cx - 19, 20, cx + 19, MOUTH_H - 3), fill=CAT["mouth_in"])
    d.ellipse(sbox(cx - 19, 20, cx + 19, MOUTH_H - 3), outline=CAT["line"], width=round(1.8 * SS))
    d.ellipse(sbox(cx - 11, 37, cx + 11, MOUTH_H + 4), fill=CAT["tongue"])
    d.ellipse(sbox(cx - 19, 20, cx + 19, MOUTH_H - 3), outline=CAT["line"], width=round(1.8 * SS))
    for s in (-1, 1):
        fx = cx + s * 11
        d.polygon(spts([(fx - 3.5, 22), (fx + 3.5, 22), (fx, 31)]), fill=CAT["fang"])
    return im


def gen_cat(out):
    _save(cat_face(), out, "cat_face.png", FACE_W, FACE_H)
    _save(_eye_open(CAT["iris_top"], CAT["iris_bot"], CAT["line"], "slit", (20, 22, 28, 255)),
          out, "cat_eye_open.png", EYE, EYE)
    _save(_eye_closed(CAT["line"]), out, "cat_eye_closed.png", EYE, EYE)
    _save(cat_mouth_closed(), out, "cat_mouth_closed.png", MOUTH_W, MOUTH_H)
    _save(cat_mouth_open(), out, "cat_mouth_open.png", MOUTH_W, MOUTH_H)


# ------------------------------------------------------------------ bat

BAT = {
    "edge": (96, 90, 150, 255),
    "core": (142, 136, 192, 255),
    "hl": (172, 166, 216, 90),
    "socket": (70, 64, 118, 85),
    "ear_in": (66, 50, 92, 255),
    "ear_glow": (150, 104, 140, 120),
    "chin": (238, 228, 214, 255),
    "blush": (208, 130, 160, 70),
    "line": (36, 28, 48, 255),
    "iris_top": (94, 66, 110, 255),
    "iris_bot": (52, 36, 66, 255),
    "ring": (242, 176, 74, 255),
    "nose": (70, 52, 88, 255),
    "mouth_in": (52, 30, 52, 255),
    "tongue": (226, 122, 142, 255),
    "fang": (246, 246, 242, 255),
}


def bat_face():
    P = BAT
    cx = FACE_W // 2

    # Enormous round ears — the bat's signature.
    ears = canvas(FACE_W, FACE_H)
    de = ImageDraw.Draw(ears)
    for s in (-1, 1):
        bx = cx + s * 74
        side_a = qbez(*spts([(bx - s * 52, 112), (bx - s * 54, 30), (bx + s * 8, 4)]))
        side_b = qbez(*spts([(bx + s * 8, 4), (bx + s * 52, 36), (bx + s * 52, 108)]))
        de.polygon(side_a + side_b, fill=P["edge"])
    for s in (-1, 1):
        bx = cx + s * 74
        side_a = qbez(*spts([(bx - s * 32, 104), (bx - s * 34, 44), (bx + s * 6, 22)]))
        side_b = qbez(*spts([(bx + s * 6, 22), (bx + s * 36, 48), (bx + s * 36, 100)]))
        inner = Image.new("RGBA", ears.size, CLEAR)
        ImageDraw.Draw(inner).polygon(side_a + side_b, fill=P["ear_in"])
        ears = Image.alpha_composite(ears, inner)
        ears = glow(ears, bx + s * 2, 92, 20, 24, P["ear_glow"], blur=10)

    # Round fuzzy head with zigzag cheek fluff.
    mask = Image.new("L", ears.size, 0)
    dm = ImageDraw.Draw(mask)
    dm.rounded_rectangle(sbox(cx - 124, 58, cx + 124, 330), radius=105 * SS, fill=255)
    for s in (-1, 1):
        dm.polygon(spts([(cx + s * 112, 140), (cx + s * 142, 132), (cx + s * 128, 158),
                         (cx + s * 146, 166), (cx + s * 124, 186), (cx + s * 134, 202),
                         (cx + s * 104, 206)]), fill=255)
    head = colorized(mask, P["edge"], P["core"])
    head = glow(head, cx, 92, 104, 66, P["hl"], blur=13)
    head = eye_sockets(head, P["socket"])
    # cream chin/muzzle patch (sits behind the mouth tile)
    head = patch(head, cx, 180, 46, 30, P["chin"], blur=2.0)
    for s in (-1, 1):
        head = glow(head, cx + s * 80, 150, 26, 16, P["blush"][:3] + (50,), blur=10)

    return Image.alpha_composite(ears, head)


def bat_mouth_closed():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    d.rounded_rectangle(sbox(cx - 7, 6, cx + 7, 15), radius=4 * SS, fill=BAT["nose"])
    d.ellipse(sbox(cx - 5, 8, cx - 2, 10), fill=(255, 255, 255, 70))
    tapered_stroke(d, (cx - 16, 25), (cx - 8, 33), (cx, 26), 1.2, 3.2, 2.0, BAT["line"])
    tapered_stroke(d, (cx, 26), (cx + 8, 33), (cx + 16, 25), 2.0, 3.2, 1.2, BAT["line"])
    for s in (-1, 1):  # tiny fangs peeking from the smile
        fx = cx + s * 9
        d.polygon(spts([(fx - 3, 29.5), (fx + 3, 29.5), (fx, 37)]), fill=BAT["fang"])
    return im


def bat_mouth_open():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    d.rounded_rectangle(sbox(cx - 7, 2, cx + 7, 11), radius=4 * SS, fill=BAT["nose"])
    d.ellipse(sbox(cx - 17, 17, cx + 17, MOUTH_H - 4), fill=BAT["mouth_in"])
    d.ellipse(sbox(cx - 10, 35, cx + 10, MOUTH_H + 4), fill=BAT["tongue"])
    d.ellipse(sbox(cx - 17, 17, cx + 17, MOUTH_H - 4), outline=BAT["line"], width=round(1.8 * SS))
    for s in (-1, 1):
        fx = cx + s * 9
        d.polygon(spts([(fx - 3.5, 19), (fx + 3.5, 19), (fx, 29)]), fill=BAT["fang"])
    return im


def gen_bat(out):
    _save(bat_face(), out, "bat_face.png", FACE_W, FACE_H)
    _save(_eye_open(BAT["iris_top"], BAT["iris_bot"], BAT["line"], "round", (26, 18, 34, 255),
                    glint_big=7, ring=BAT["ring"]),
          out, "bat_eye_open.png", EYE, EYE)
    _save(_eye_closed(BAT["line"], lashes=False), out, "bat_eye_closed.png", EYE, EYE)
    _save(bat_mouth_closed(), out, "bat_mouth_closed.png", MOUTH_W, MOUTH_H)
    _save(bat_mouth_open(), out, "bat_mouth_open.png", MOUTH_W, MOUTH_H)


# ------------------------------------------------------------------ toad

TOAD = {
    "edge": (104, 128, 74, 255),
    "core": (150, 176, 104, 255),
    "hl": (182, 204, 134, 95),
    "socket": (78, 100, 54, 85),
    "belly": (214, 227, 172, 255),
    "spot": (88, 110, 62, 140),
    "blush": (196, 140, 120, 70),
    "line": (44, 54, 34, 255),
    "iris_top": (248, 214, 120, 255),
    "iris_bot": (222, 160, 66, 255),
    "mouth_in": (62, 44, 34, 255),
    "tongue": (226, 130, 140, 255),
}


def toad_face():
    P = TOAD
    cx = FACE_W // 2

    # Wide squat head with eye bumps merged into the silhouette.
    mask = Image.new("L", canvas(FACE_W, FACE_H).size, 0)
    dm = ImageDraw.Draw(mask)
    dm.rounded_rectangle(sbox(cx - 150, 80, cx + 150, 330), radius=100 * SS, fill=255)
    for ex in (96, 224):  # eye bumps under the eye tiles
        dm.ellipse(sbox(ex - 48, 42, ex + 48, 132), fill=255)
    head = colorized(mask, P["edge"], P["core"])
    head = glow(head, cx, 100, 130, 70, P["hl"], blur=14)
    head = eye_sockets(head, P["socket"], y=98, rx=40, ry=34)
    # lighter belly / chin
    head = patch(head, cx, 214, 86, 52, P["belly"], blur=3.0)

    # mottled spots
    def spots(d):
        for (sx, sy, rx, ry) in [(70, 150, 9, 7), (250, 146, 10, 7), (160, 70, 8, 6),
                                 (52, 190, 7, 5), (268, 192, 7, 5), (118, 52, 6, 5),
                                 (204, 50, 6, 5)]:
            d.ellipse(sbox(sx - rx, sy - ry, sx + rx, sy + ry), fill=P["spot"])
    head = clip_draw(head, spots, blur=1.5)
    for s in (-1, 1):
        head = glow(head, cx + s * 96, 162, 26, 15, P["blush"][:3] + (40,), blur=11)
    return head


def toad_mouth_closed():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    for s in (-1, 1):  # nostril dots, no snout
        d.ellipse(sbox(cx + s * 7 - 2, 8, cx + s * 7 + 2, 12), fill=TOAD["line"])
    # the big contented toad smile — one wide shallow curve with upturned ends
    tapered_stroke(d, (cx - 38, 26), (cx, 40), (cx + 38, 26), 2.0, 4.0, 2.0, TOAD["line"])
    tapered_stroke(d, (cx - 38, 26), (cx - 42, 24), (cx - 44, 20), 2.0, 1.4, 0.8, TOAD["line"])
    tapered_stroke(d, (cx + 38, 26), (cx + 42, 24), (cx + 44, 20), 2.0, 1.4, 0.8, TOAD["line"])
    return im


def toad_mouth_open():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    for s in (-1, 1):
        d.ellipse(sbox(cx + s * 7 - 2, 4, cx + s * 7 + 2, 8), fill=TOAD["line"])
    d.rounded_rectangle(sbox(cx - 30, 16, cx + 30, MOUTH_H - 4), radius=16 * SS,
                        fill=TOAD["mouth_in"])
    d.ellipse(sbox(cx - 16, 34, cx + 16, MOUTH_H + 6), fill=TOAD["tongue"])
    d.rounded_rectangle(sbox(cx - 30, 16, cx + 30, MOUTH_H - 4), radius=16 * SS,
                        outline=TOAD["line"], width=round(1.8 * SS))
    return im


def gen_toad(out):
    _save(toad_face(), out, "toad_face.png", FACE_W, FACE_H)
    _save(_eye_open(TOAD["iris_top"], TOAD["iris_bot"], TOAD["line"], "bar", (30, 24, 18, 255)),
          out, "toad_eye_open.png", EYE, EYE)
    _save(_eye_closed(TOAD["line"], lashes=False), out, "toad_eye_closed.png", EYE, EYE)
    _save(toad_mouth_closed(), out, "toad_mouth_closed.png", MOUTH_W, MOUTH_H)
    _save(toad_mouth_open(), out, "toad_mouth_open.png", MOUTH_W, MOUTH_H)


# ------------------------------------------------------------------ fox

FOX = {
    "edge": (176, 100, 50, 255),
    "core": (226, 141, 78, 255),
    "hl": (243, 172, 108, 100),
    "socket": (138, 76, 40, 80),
    "ear_tip": (58, 44, 42, 255),
    "ear_in": (240, 222, 200, 255),
    "ruff": (240, 228, 210, 255),
    "ruff_dk": (206, 188, 164, 255),
    "muzzle": (243, 231, 212, 235),
    "blush": (222, 120, 100, 60),
    "line": (46, 36, 34, 255),
    "iris_top": (247, 196, 96, 255),
    "iris_bot": (206, 126, 46, 255),
    "nose": (56, 44, 42, 255),
    "mouth_in": (74, 40, 40, 255),
    "tongue": (234, 138, 140, 255),
    "tooth": (248, 246, 240, 255),
}


def fox_face():
    P = FOX
    cx = FACE_W // 2

    # Tall pointed ears with dark tips and cream inner.
    ears = canvas(FACE_W, FACE_H)
    de = ImageDraw.Draw(ears)
    for s in (-1, 1):
        bx = cx + s * 96
        side_a = qbez(*spts([(bx - s * 46, 110), (bx - s * 34, 36), (bx + s * 2, 6)]))
        side_b = qbez(*spts([(bx + s * 2, 6), (bx + s * 30, 42), (bx + s * 44, 102)]))
        de.polygon(side_a + side_b, fill=P["edge"])
    # dark tips: clip a band across the top of the ears
    ears = clip_draw(ears, lambda d: d.rectangle(sbox(0, 0, FACE_W, 36), fill=P["ear_tip"]),
                     blur=1.2)
    for s in (-1, 1):
        bx = cx + s * 96
        side_a = qbez(*spts([(bx - s * 26, 100), (bx - s * 18, 58), (bx + s * 2, 44)]))
        side_b = qbez(*spts([(bx + s * 2, 44), (bx + s * 18, 60), (bx + s * 26, 96)]))
        inner = Image.new("RGBA", ears.size, CLEAR)
        ImageDraw.Draw(inner).polygon(side_a + side_b, fill=P["ear_in"])
        ears = Image.alpha_composite(ears, inner)

    # Head + big cream cheek ruffs (drawn into the same silhouette, recolored after).
    mask = Image.new("L", ears.size, 0)
    dm = ImageDraw.Draw(mask)
    dm.rounded_rectangle(sbox(cx - 128, 50, cx + 128, 330), radius=110 * SS, fill=255)
    ruff_mask = Image.new("L", ears.size, 0)
    dr = ImageDraw.Draw(ruff_mask)
    for s in (-1, 1):
        dr.polygon(spts([(cx + s * 92, 122), (cx + s * 152, 128), (cx + s * 130, 154),
                         (cx + s * 158, 166), (cx + s * 126, 186), (cx + s * 138, 206),
                         (cx + s * 88, 210), (cx + s * 84, 150)]), fill=255)
    mask = ImageChops.lighter(mask, ruff_mask)
    head = colorized(mask, P["edge"], P["core"])
    head = glow(head, cx, 84, 112, 68, P["hl"], blur=13)

    # cream ruffs recolored on top (crisp spikes, gently shaded)
    ruff = colorized(ruff_mask, P["ruff_dk"], P["ruff"], shrink=4, blur=3)
    head = Image.alpha_composite(head, ruff)

    head = eye_sockets(head, P["socket"])
    # cream muzzle patch behind the mouth tile
    head = patch(head, cx, 178, 48, 32, P["muzzle"], blur=2.0)
    for s in (-1, 1):
        head = glow(head, cx + s * 76, 146, 24, 14, P["blush"][:3] + (50,), blur=9)
    return Image.alpha_composite(ears, head)


def _fox_nose(d, cx, top, P):
    body = qbez(*spts([(cx - 9, top), (cx, top + 2), (cx + 9, top)])) + \
           qbez(*spts([(cx + 9, top), (cx + 6, top + 8), (cx, top + 11)])) + \
           qbez(*spts([(cx, top + 11), (cx - 6, top + 8), (cx - 9, top)]))
    d.polygon(body, fill=P["nose"])
    d.ellipse(sbox(cx - 5, top + 2, cx - 1, top + 5), fill=(255, 255, 255, 80))


def fox_mouth_closed():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _fox_nose(d, cx, 5, FOX)
    d.line(spts([(cx, 16), (cx, 22)]), fill=FOX["line"], width=round(1.6 * SS))
    tapered_stroke(d, (cx - 18, 23), (cx - 9, 32), (cx, 24), 1.2, 3.2, 2.0, FOX["line"])
    tapered_stroke(d, (cx, 24), (cx + 9, 32), (cx + 18, 23), 2.0, 3.2, 1.2, FOX["line"])
    return im


def fox_mouth_open():
    im = canvas(MOUTH_W, MOUTH_H)
    d = ImageDraw.Draw(im)
    cx = MOUTH_W // 2
    _fox_nose(d, cx, 2, FOX)
    d.ellipse(sbox(cx - 18, 18, cx + 18, MOUTH_H - 3), fill=FOX["mouth_in"])
    d.ellipse(sbox(cx - 10, 36, cx + 10, MOUTH_H + 4), fill=FOX["tongue"])
    d.ellipse(sbox(cx - 18, 18, cx + 18, MOUTH_H - 3), outline=FOX["line"], width=round(1.8 * SS))
    d.polygon(spts([(cx - 12, 20), (cx - 5, 20), (cx - 8.5, 28)]), fill=FOX["tooth"])
    return im


def gen_fox(out):
    _save(fox_face(), out, "fox_face.png", FACE_W, FACE_H)
    _save(_eye_open(FOX["iris_top"], FOX["iris_bot"], FOX["line"], "slit", (32, 24, 22, 255)),
          out, "fox_eye_open.png", EYE, EYE)
    _save(_eye_closed(FOX["line"]), out, "fox_eye_closed.png", EYE, EYE)
    _save(fox_mouth_closed(), out, "fox_mouth_closed.png", MOUTH_W, MOUTH_H)
    _save(fox_mouth_open(), out, "fox_mouth_open.png", MOUTH_W, MOUTH_H)


# ------------------------------------------------------------------ output

FAMILIARS = {"cat": gen_cat, "bat": gen_bat, "toad": gen_toad, "fox": gen_fox}


def _save(im, out, name, w, h):
    path = os.path.join(out, name)
    down(im, w, h).save(path, optimize=True)


def preview_sheet(out, path):
    """Contact sheet: each familiar composited at the firmware offsets, in
    neutral / blink / talking states, on the black panel background."""
    states = [("neutral", "eye_open", "mouth_closed"),
              ("blink", "eye_closed", "mouth_closed"),
              ("talking", "eye_open", "mouth_open")]
    pad = 8
    sheet = Image.new("RGB", ((FACE_W + pad) * len(states) + pad,
                              (FACE_H + pad) * len(FAMILIARS) + pad), (24, 24, 28))
    for row, name in enumerate(FAMILIARS):
        sprites = {k: Image.open(os.path.join(out, f"{name}_{k}.png")).convert("RGBA")
                   for k in ("face", "eye_open", "eye_closed", "mouth_closed", "mouth_open")}
        for col, (_, eye, mouth) in enumerate(states):
            panel = Image.new("RGBA", (FACE_W, FACE_H), (0, 0, 0, 255))
            panel.alpha_composite(sprites["face"])
            pcx, pcy = FACE_W // 2, FACE_H // 2
            for ex in (-64, 64):  # firmware: eye centers at (+-64, -20), mouth (0, +44)
                panel.alpha_composite(sprites[eye], (pcx + ex - EYE // 2, pcy - 20 - EYE // 2))
            panel.alpha_composite(sprites[mouth], (pcx - MOUTH_W // 2, pcy + 44 - MOUTH_H // 2))
            sheet.paste(panel.convert("RGB"),
                        (pad + col * (FACE_W + pad), pad + row * (FACE_H + pad)))
    sheet.save(path)
    print("wrote preview", path)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    default_out = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "assets_bin"))
    ap.add_argument("--out", default=default_out)
    ap.add_argument("--preview", default=None, metavar="FILE.png")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    for name, gen in FAMILIARS.items():
        gen(args.out)
        sizes = []
        for f in sorted(os.listdir(args.out)):
            if f.startswith(name + "_"):
                sizes.append(f"{f} {os.path.getsize(os.path.join(args.out, f))}B")
        print(" ", "  ".join(sizes))
    if args.preview:
        preview_sheet(args.out, args.preview)


if __name__ == "__main__":
    main()
