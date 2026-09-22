#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""UptimeMesh 品牌标识生成器(单一事实源:一套几何 + 色板同时产出 SVG 与位图)。

    python docs/logo/generate_logo.py            # 全部产物
    python docs/logo/generate_logo.py --only svg # 只出矢量
    python docs/logo/generate_logo.py --check    # 只跑设计自检

产出(均在本目录):
    uptimemesh-mark.svg                     裸图形(浅底通用)
    uptimemesh-mark-on-dark.svg             裸图形(深底版,无底板)
    uptimemesh-mark-compact.svg             裸图形简化版(33~127px)
    uptimemesh-mark-badge.svg               带圆角方底板(头像/Logo 位)
    uptimemesh-mark-badge-compact.svg       带底板简化版
    uptimemesh-logo-horizontal.svg          横向主锁定版(浅底)
    uptimemesh-logo-horizontal-inverse.svg  横向主锁定版(深底)
    uptimemesh-icon-{512,256,128,64,48,32,24,16}.png
    uptimemesh-favicon.ico                  16/24/32/48 多帧
    uptimemesh-logo-horizontal{,-inverse}.png
    uptimemesh-mark-on-{light,dark}.png     预览
    uptimemesh-og-banner.png                1584×704 社交分享图
    uptimemesh-brand-sheet.png              交付总览(验收用)

设计口径见 docs/logo/README.md。字标由 Cascadia Code(SIL OFL 1.1)
转曲线嵌入 SVG,位图侧走同一字体同一字重,两边永远一致。
"""

from __future__ import annotations

import argparse
import copy
import io
import math
import os
import re
import xml.etree.ElementTree as ET
from dataclasses import dataclass

import numpy as np
from PIL import Image, ImageDraw, ImageFont

from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.misc.transform import Transform

HERE = os.path.dirname(os.path.abspath(__file__))

# ==========================================================================
# 色板 —— 与仪表盘同源:品牌蓝提炼自 Element Plus primary #409eff,
# up/breach 状态色直接沿用(#67c23a / #e6a23c → 提纯为 MINT / AMBER)。
# ==========================================================================
NAVY_TOP = "#0B1E33"   # 底板渐变上端
NAVY_BOT = "#173C5C"   # 底板渐变下端
BLUE = "#2D7FF9"       # 品牌蓝(mesh 连线)
BLUE_DEEP = "#1B5FD9"  # 节点中心
CYAN = "#5CD3FF"       # 节点高光 / 底板描边
MINT = "#3DDC84"       # 脉冲(uptime green)
AMBER = "#F5A623"      # 报警节点
INK = "#0E2233"        # 浅底字标主色
SLATE = "#53718C"      # 浅底副标语
SNOW = "#F2F8FF"       # 深底字标主色
ICE = "#8FC4FF"        # 深底字标次色 / 副标语
DIM = "#9DBBDD"        # 深底辅助文字

FONT_PATH = os.environ.get("UM_LOGO_FONT", r"C:\Windows\Fonts\CascadiaCode.ttf")
W_MAIN = 650          # 字标字重
W_SUB = 500           # 副标字重
TRACK_MAIN = -0.012   # 字标字距(em)
TRACK_SUB = 0.20      # 副标字距(em)

WORD_MAIN = "UptimeMesh"
SPLIT_AT = 6                   # "Uptime" | "Mesh" 换色处
SUB_TEXT = "DISTRIBUTED ACTIVE MONITORING"


# ==========================================================================
# 图形几何(512×512 画布为单位)
#   六边形 = 节点网格(mesh),中心 = Dashboard,
#   横贯的心电线 = 主动探测(uptime),右下节点琥珀色 = 正在报警。
#
#   三套参数:
#     FULL    —— 主版,细节完整(≥128px)
#     COMPACT —— 简化版:线宽加粗、节点放大、去掉对角细线(33~127px)
#     MICRO   —— 极限版:只留"网格 + 中心 + 报警节点",16~32px 专用,
#                否则中心白点会在抗锯齿下直接消失(实测过)。
# ==========================================================================
@dataclass(frozen=True)
class MarkParams:
    size: float = 512.0
    cx: float = 256.0
    cy: float = 258.0
    r: float = 138.0             # 六边形外接圆半径
    node_r: float = 21.0         # 节点半径
    ring_w: float = 9.0          # 网格边线宽
    diag_w: float = 7.0          # 对角连线宽
    diagonals_on: bool = True
    hub_r: float = 27.0          # 中心枢纽半径
    hub_w: float = 9.0           # 中心枢纽环宽(0 ⇒ 实心点)
    pulse_w: float = 15.0        # 脉冲线宽(0 ⇒ 不画)
    pulse_cut: float = 22.0      # 脉冲挖空宽(压在底板上让脉冲"穿过"网格)
    alert_node: int = 2          # 琥珀色节点索引(右下)
    plate_pad: float = 16.0      # 底板内缩
    plate_r: float = 112.0       # 底板圆角
    pulse: tuple = (
        (64, 258), (146, 258), (172, 232), (196, 286), (222, 182),
        (250, 330), (278, 258), (338, 258), (358, 242), (378, 258), (448, 258),
    )

    @property
    def vertices(self) -> list:
        return [
            (self.cx + self.r * math.cos(math.radians(a)),
             self.cy + self.r * math.sin(math.radians(a)))
            for a in (-90, -30, 30, 90, 150, 210)
        ]

    @property
    def diagonals(self) -> list:
        if not self.diagonals_on:
            return []
        v = self.vertices
        # 只连两条对角(不连竖直那条):避免与脉冲峰在同一处交叉糊掉
        return [(v[1], v[4]), (v[2], v[5])]


FULL = MarkParams()
COMPACT = MarkParams(
    r=126.0, node_r=30.0, ring_w=16.0, diag_w=0.0, diagonals_on=False,
    hub_r=20.0, hub_w=0.0, pulse_w=26.0, pulse_cut=42.0,
    plate_pad=20.0, plate_r=118.0,
    pulse=((78, 258), (168, 258), (216, 150), (266, 366), (300, 258), (434, 258)),
)
MICRO = MarkParams(
    r=112.0, node_r=36.0, ring_w=34.0, diag_w=0.0, diagonals_on=False,
    hub_r=40.0, hub_w=0.0, pulse_w=0.0, pulse_cut=0.0,
    plate_pad=18.0, plate_r=126.0, pulse=(),
)


# ==========================================================================
# 配色方案:同一几何,三种落位(浅底裸 / 深底裸 / 深色底板)
# ==========================================================================
@dataclass(frozen=True)
class Scheme:
    dark: bool                    # 是否用于深色背景(连线/节点走渐变)
    link: str                     # 位图实色(与渐变主色一致)
    hub: str
    node: str
    node_stroke: str
    on_plate: bool = False        # 画在底板上 ⇒ 脉冲要做挖空


SCHEME_LIGHT = Scheme(False, BLUE, INK, BLUE_DEEP, "#BBD9FF")
SCHEME_DARK = Scheme(True, BLUE, SNOW, BLUE_DEEP, CYAN)
SCHEME_PLATE = Scheme(True, BLUE, SNOW, BLUE_DEEP, CYAN, on_plate=True)


# ==========================================================================
# 字体层:可变字体实例化 + 逐字形排版(SVG 曲线与位图共用同一实例)
# ==========================================================================
class Face:
    def __init__(self, path: str = FONT_PATH):
        if not os.path.exists(path):
            raise SystemExit(f"找不到字体 {path};用 UM_LOGO_FONT 指定含拉丁字形的 ttf")
        self.path = path
        self.source = TTFont(path)
        self.has_fvar = "fvar" in self.source
        self._cache: dict[int, TTFont] = {}
        self._raw: bytes | None = None

    def instance(self, weight: int) -> TTFont:
        if weight not in self._cache:
            f = copy.deepcopy(self.source)
            if self.has_fvar:
                f = instancer.instantiateVariableFont(f, {"wght": weight})
            self._cache[weight] = f
        return self._cache[weight]

    def pil_font(self, weight: int, px: int) -> ImageFont.FreeTypeFont:
        """Pillow 侧同字重字体。每次从内存流新建:Pillow 按路径缓存字体对象,
        而 set_variation_by_axes 是原地改状态,共用缓存会跨字重互相污染。"""
        if self._raw is None:
            with open(self.path, "rb") as fh:
                self._raw = fh.read()
        font = ImageFont.FreeTypeFont(font=io.BytesIO(self._raw), size=px)
        if self.has_fvar:
            font.set_variation_by_axes([weight])
        return font

    def layout(self, text: str, size: float, weight: int, track_em: float):
        """逐字排版 → (总宽, [(glyph, x_offset, adv)], cap 高)。"""
        f = self.instance(weight)
        upem = f["head"].unitsPerEm
        cmap = f.getBestCmap()
        hmtx = f["hmtx"]
        s = size / upem
        track = track_em * size
        x, placed = 0.0, []
        for ch in text:
            if ord(ch) not in cmap:
                raise SystemExit(f"字体缺少字形 {ch!r}")
            g = cmap[ord(ch)]
            adv = hmtx[g][0] * s
            placed.append((g, x, adv))
            x += adv + track
        os2 = f["OS/2"]
        cap = getattr(os2, "sCapHeight", 0) or os2.sTypoAscender
        return x - track, placed, cap * s

    def glyph_d(self, gname: str, size: float, weight: int, ox: float, baseline: float) -> str:
        """单字形 → SVG path(缩放到 size、落到基线、y 轴翻转)。"""
        f = self.instance(weight)
        s = size / f["head"].unitsPerEm
        pen = SVGPathPen(f.getGlyphSet())
        f.getGlyphSet()[gname].draw(
            TransformPen(pen, Transform(s, 0, 0, -s, ox, baseline)))
        return pen.getCommands().strip()


FACE = Face()


# ==========================================================================
# SVG 输出
# ==========================================================================
def pts_str(pts) -> str:
    return " ".join(f"{a:.2f},{b:.2f}" for a, b in pts)


def header(w: float, h: float) -> str:
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w:g}" height="{h:g}" '
            f'viewBox="0 0 {w:g} {h:g}" role="img" aria-label="UptimeMesh">')


# 渐变全部用 userSpaceOnUse(图形局部 0..512 坐标),这样底板矩形与
# 脉冲挖空采样同一个渐变场;位图侧 gradient() 用同样的投影公式,两边一致。
DEFS = f"""
  <defs>
    <linearGradient id="um-plate" gradientUnits="userSpaceOnUse"
                    x1="0" y1="0" x2="{0.35 * 512:.2f}" y2="512">
      <stop offset="0" stop-color="{NAVY_TOP}"/><stop offset="1" stop-color="{NAVY_BOT}"/>
    </linearGradient>
    <linearGradient id="um-link" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="{BLUE}"/><stop offset="1" stop-color="{CYAN}"/>
    </linearGradient>
    <linearGradient id="um-pulse" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="{MINT}"/><stop offset="0.55" stop-color="#7BEBAE"/>
      <stop offset="1" stop-color="{MINT}"/>
    </linearGradient>
    <radialGradient id="um-node" cx="0.35" cy="0.3" r="0.85">
      <stop offset="0" stop-color="{CYAN}"/><stop offset="1" stop-color="{BLUE_DEEP}"/>
    </radialGradient>
  </defs>"""


def plate_rect(m: MarkParams) -> str:
    s, p = m.size, m.plate_pad
    return (f'<rect x="{p:g}" y="{p:g}" width="{s - 2 * p:g}" height="{s - 2 * p:g}" '
            f'rx="{m.plate_r:g}" fill="url(#um-plate)"/>\n'
            f'  <rect x="{p:g}" y="{p:g}" width="{s - 2 * p:g}" height="{s - 2 * p:g}" '
            f'rx="{m.plate_r:g}" fill="none" stroke="{CYAN}" stroke-width="2" '
            f'opacity="0.28"/>')


def mark_body(m: MarkParams, sc: Scheme) -> str:
    v = m.vertices
    link = "url(#um-link)" if sc.dark else sc.link
    node = "url(#um-node)" if sc.dark else sc.node
    out = []
    for a, b in m.diagonals:
        out.append(f'<line x1="{a[0]:.1f}" y1="{a[1]:.1f}" x2="{b[0]:.1f}" y2="{b[1]:.1f}" '
                   f'stroke="{link}" stroke-width="{m.diag_w}" stroke-linecap="round" '
                   f'opacity="{0.55 if sc.dark else 0.4}"/>')
    out.append(f'<polygon points="{pts_str(v + [v[0]])}" fill="none" stroke="{link}" '
               f'stroke-width="{m.ring_w}" stroke-linejoin="round"/>')
    if m.hub_w:
        out.append(f'<circle cx="{m.cx}" cy="{m.cy}" r="{m.hub_r}" fill="none" '
                   f'stroke="{sc.hub}" stroke-width="{m.hub_w}"/>')
    else:
        out.append(f'<circle cx="{m.cx}" cy="{m.cy}" r="{m.hub_r}" fill="{sc.hub}"/>')
    for i, (px, py) in enumerate(v):
        fill, stroke = (AMBER, "#FFD79A") if i == m.alert_node else (node, sc.node_stroke)
        out.append(f'<circle cx="{px:.1f}" cy="{py:.1f}" r="{m.node_r}" fill="{fill}" '
                   f'stroke="{stroke}" stroke-width="4"/>')
    if m.pulse_w and m.pulse:
        pulse = pts_str(m.pulse)
        if sc.on_plate and m.pulse_cut:
            # 挖空色与底板同一支渐变,缝才不会在渐变亮端留下补丁痕
            out.append(f'<polyline points="{pulse}" fill="none" stroke="url(#um-plate)" '
                       f'stroke-width="{m.pulse_cut}" stroke-linecap="round" '
                       f'stroke-linejoin="round"/>')
        out.append(f'<polyline points="{pulse}" fill="none" stroke="url(#um-pulse)" '
                   f'stroke-width="{m.pulse_w}" stroke-linecap="round" '
                   f'stroke-linejoin="round"/>')
    return "\n    ".join(out)


def badge_svg(m: MarkParams = FULL) -> str:
    return header(m.size, m.size) + DEFS + f"""
  {plate_rect(m)}
    {mark_body(m, SCHEME_PLATE)}
</svg>
"""


def bare_svg(m: MarkParams = FULL, sc: Scheme = SCHEME_LIGHT) -> str:
    return header(m.size, m.size) + DEFS + f"""
    {mark_body(m, sc)}
</svg>
"""


# ---- 横向主锁定版:全部尺寸由图形边长按光学比例推导,SVG 与位图共用 ----
LOCK_MARK = 260          # 图形边长(基准)
LOCK_PAD_RATIO = 0.155   # 画布留白 / 图形
LOCK_GAP_RATIO = 0.177   # 图形 ↔ 字标 / 图形
LOCK_CAP_RATIO = 0.40    # 字标 cap 高 ÷ 图形(光学等重)
LOCK_SUB_RATIO = 0.135   # 副标字号 / 图形
LOCK_LEAD_RATIO = 0.30   # 字标基线 → 副标基线 / 图形


def lockup_metrics(mark: float = LOCK_MARK) -> dict:
    pad = mark * LOCK_PAD_RATIO
    gap = mark * LOCK_GAP_RATIO
    x_text = pad + mark + gap

    # 字号由目标 cap 反解(cap/size 比从字体里读,换字体不用改参数)
    def size_for_cap(target: float, weight: int, track: float):
        _w, _p, cap100 = FACE.layout(WORD_MAIN, 100.0, weight, track)
        size = target / (cap100 / 100.0)
        total, placed, cap = FACE.layout(WORD_MAIN, size, weight, track)
        return size, total, placed, cap

    main_size, main_w, main_placed, cap = size_for_cap(mark * LOCK_CAP_RATIO, W_MAIN, TRACK_MAIN)
    sub_size = mark * LOCK_SUB_RATIO
    sub_w, sub_placed, _sc = FACE.layout(SUB_TEXT, sub_size, W_SUB, TRACK_SUB)

    total_h = pad * 2 + mark
    total_w = x_text + max(main_w, sub_w) + pad
    block = cap + mark * LOCK_LEAD_RATIO + sub_size * 0.72
    base_main = (total_h - block) / 2 + cap     # 文字块整体对图形做光学居中
    base_sub = base_main + mark * LOCK_LEAD_RATIO
    return dict(mark=mark, pad=pad, gap=gap, x_text=x_text,
                main_size=main_size, main_w=main_w, main_placed=main_placed, cap=cap,
                sub_size=sub_size, sub_w=sub_w, sub_placed=sub_placed,
                total_w=total_w, total_h=total_h,
                base_main=base_main, base_sub=base_sub)


def lockup_svg(inverse: bool = False) -> str:
    m = FULL
    k = lockup_metrics()
    main_colors = (SNOW, ICE) if inverse else (INK, BLUE)
    sub_color = ICE if inverse else SLATE

    scale = k["mark"] / m.size
    out = [header(round(k["total_w"], 2), round(k["total_h"], 2)), DEFS]
    out.append(f'<g transform="translate({k["pad"]:.1f},'
               f'{(k["total_h"] - k["mark"]) / 2:.1f}) scale({scale:.5f})">')
    if not inverse:
        out.append("  " + plate_rect(m))
    out.append("    " + mark_body(m, SCHEME_PLATE if not inverse else SCHEME_DARK))
    out.append("</g>")

    for i, (g, off, _a) in enumerate(k["main_placed"]):
        d = FACE.glyph_d(g, k["main_size"], W_MAIN, k["x_text"] + off, k["base_main"])
        if d:
            out.append(f'<path d="{d}" fill="'
                       f'{main_colors[0] if i < SPLIT_AT else main_colors[1]}"/>')
    for g, off, _a in k["sub_placed"]:
        d = FACE.glyph_d(g, k["sub_size"], W_SUB, k["x_text"] + off, k["base_sub"])
        if d:
            out.append(f'<path d="{d}" fill="{sub_color}"/>')
    out.append("</svg>")
    return "\n".join(out) + "\n"


# ==========================================================================
# 位图渲染(与 SVG 同一套几何与渐变语义)
# ==========================================================================
SS = 4  # 超采样


def hx(c: str) -> tuple:
    c = c.lstrip("#")
    return tuple(int(c[i:i + 2], 16) for i in (0, 2, 4)) + (255,)


def _stops_rgb(stops, t):
    """分段线性插值stops:[(offset, hex), ...] → RGB。"""
    t = min(max(t, 0.0), 1.0)
    for (o0, c0), (o1, c1) in zip(stops, stops[1:]):
        if o0 <= t <= o1:
            f = 0.0 if o1 == o0 else (t - o0) / (o1 - o0)
            a, b = np.array(hx(c0)[:3]), np.array(hx(c1)[:3])
            return tuple(int(round(x)) for x in a * (1 - f) + b * f)
    return hx(stops[-1][1])[:3]


def paint_gradient(img: Image.Image, mask: Image.Image, bbox, vec: tuple,
                   stops, alpha: int = 255) -> None:
    """按 SVG objectBoundingBox 线性渐变语义,用 mask 把渐变色刷进 img。

    bbox = 元素几何包围盒(不含描边),(x0,y0,x1,y1);vec 为渐变向量。
    位图与 SVG 因此走同一个受光方向,而不是各猜一次。
    """
    x0, y0, x1, y1 = bbox
    if x1 - x0 < 1 or y1 - y0 < 1:
        return
    w, h = img.size
    cx0, cy0 = max(0, int(math.floor(x0))), max(0, int(math.floor(y0)))
    cx1, cy1 = min(w, int(math.ceil(x1)) + 1), min(h, int(math.ceil(y1)) + 1)
    if cx1 - cx0 < 1 or cy1 - cy0 < 1:
        return
    gx, gy = vec
    denom = gx * gx + gy * gy
    u = ((np.arange(cx0, cx1) - x0) / (x1 - x0)).astype(np.float32)
    v = ((np.arange(cy0, cy1) - y0) / (y1 - y0)).astype(np.float32)
    t = np.clip((gx * u[None, :] + gy * v[:, None]) / denom, 0, 1)
    lut = np.array([_stops_rgb(stops, x) for x in np.linspace(0, 1, 256)], dtype=np.uint8)
    rgb = lut[np.round(t * 255).astype(np.uint8)]
    grad = Image.frombytes("RGB", (cx1 - cx0, cy1 - cy0), rgb.tobytes()).convert("RGBA")
    grad.putalpha(mask.crop((cx0, cy0, cx1, cy1)).point(lambda p: int(p * alpha / 255)))
    img.alpha_composite(grad, (cx0, cy0))


def _bbox_of(pts) -> tuple:
    xs = [p[0] for p in pts]
    ys = [p[1] for p in pts]
    return min(xs), min(ys), max(xs), max(ys)


def gradient(w: int, h: int, radius: int, c0: str, c1: str, margin: int = 0,
             vec: tuple = (0.35, 1.0)) -> Image.Image:
    """圆角矩形 + 线性渐变。

    vec 即 SVG `<linearGradient x1,y1 -> x2,y2>`(userSpaceOnUse)的向量,
    t = 点到向量的投影 / |v|²,坐标按整幅画布归一 —— 与渲染器同算法,
    底板与其上的挖空缝因此能采到完全相同的颜色。
    """
    iw, ih = w - 2 * margin, h - 2 * margin
    gx, gy = vec
    denom = gx * gx + gy * gy
    u = (np.arange(margin, margin + iw) / (w - 1))[None, :]
    v = (np.arange(margin, margin + ih) / (h - 1))[:, None]
    t = np.clip((gx * u + gy * v) / denom, 0, 1)[..., None]
    rgb = np.array(hx(c0)[:3]) * (1 - t) + np.array(hx(c1)[:3]) * t
    img = Image.frombytes("RGB", (iw, ih), rgb.astype("uint8").tobytes()).convert("RGBA")
    mask = Image.new("L", (w, h), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [margin, margin, margin + iw - 1, margin + ih - 1], radius=radius, fill=255)
    out = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    # 遮罩必须按底板自身的偏移区域裁,从 (0,0) 裁会让圆角与右下边缘错位
    out.paste(img, (margin, margin),
              mask.crop((margin, margin, margin + iw, margin + ih)))
    return out


def polyline(d: ImageDraw.ImageDraw, pts, width: int, fill):
    """圆端点 + 圆拐角折线。"""
    d.line(pts, fill=fill, width=width, joint="curve")
    rad = width / 2
    for x, y in (pts[0], pts[-1]):
        d.ellipse([x - rad, y - rad, x + rad, y + rad], fill=fill)


def _node_sprite(radius: int, light: str, deep: str) -> Image.Image:
    """径向渐变节点(对应 SVG url(#um-node),高光偏左上)。"""
    side = max(3, radius * 2)
    yy, xx = np.mgrid[0:side, 0:side].astype(float)
    t = np.clip(np.hypot(xx - side * 0.35, yy - side * 0.30) / (side * 0.85), 0, 1)
    rgb = (np.array(hx(light)[:3]) * (1 - t)[..., None]
           + np.array(hx(deep)[:3]) * t[..., None])
    sprite = Image.frombytes("RGB", (side, side), rgb.astype("uint8").tobytes()).convert("RGBA")
    mask = Image.new("L", (side, side), 0)
    ImageDraw.Draw(mask).ellipse([0, 0, side - 1, side - 1], fill=255)
    sprite.putalpha(mask)
    return sprite


def render_mark(target: int, params: MarkParams = FULL, sc: Scheme = SCHEME_PLATE,
                plate: bool = True, bg=None) -> Image.Image:
    """位图图形:`plate` 决定是否画深色底板,`sc` 决定配色(与 SVG 共用方案)。"""
    canvas = target * SS
    k = canvas / params.size
    m = params
    img = Image.new("RGBA", (canvas, canvas), bg or (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    plate_img = None
    if plate:
        pad = round(m.plate_pad * k)
        plate_img = gradient(canvas, canvas, round(m.plate_r * k), NAVY_TOP, NAVY_BOT, pad)
        img.alpha_composite(plate_img)
        # 底板描边(SVG 里那圈青色描边,位图不能漏)
        ImageDraw.Draw(img).rounded_rectangle(
            [pad, pad, canvas - pad - 1, canvas - pad - 1], radius=round(m.plate_r * k),
            outline=hx(CYAN)[:3] + (round(0.28 * 255),), width=max(1, round(2 * k)))
        d = ImageDraw.Draw(img)

    link = hx(sc.link)
    for a, b in m.diagonals:
        seg = [(a[0] * k, a[1] * k), (b[0] * k, b[1] * k)]
        width = max(2, round(m.diag_w * k))
        alpha = int(255 * (0.55 if sc.dark else 0.4))
        if sc.dark:
            # SVG 里每条 <line> 有自己的 objectBoundingBox,渐变沿该线段本身铺;
            # 位图也逐线段刷,才与浏览器一致。
            dmask = Image.new("L", (canvas, canvas), 0)
            ImageDraw.Draw(dmask).line(seg, fill=255, width=width)
            paint_gradient(img, dmask, _bbox_of(seg), (1.0, 1.0),
                           [(0.0, BLUE), (1.0, CYAN)], alpha=alpha)
        else:
            d.line(seg, fill=link[:3] + (alpha,), width=width)
    d = ImageDraw.Draw(img)
    ring = [(x * k, y * k) for x, y in m.vertices]
    # 网格边线:SVG 深底用 url(#um-link)(沿六边形包围盒对角),浅底是实色
    if sc.dark:
        seam = Image.new("L", (canvas, canvas), 0)
        sd = ImageDraw.Draw(seam)
        for i, a in enumerate(ring):
            b = ring[(i + 1) % len(ring)]
            sd.line([a, b], fill=255, width=max(2, round(m.ring_w * k)))
        paint_gradient(img, seam, _bbox_of(ring), (1.0, 1.0), [(0.0, BLUE), (1.0, CYAN)])
        d = ImageDraw.Draw(img)
    else:
        for i, a in enumerate(ring):
            b = ring[(i + 1) % len(ring)]
            d.line([a, b], fill=link, width=max(2, round(m.ring_w * k)))
    hr = m.hub_r * k
    hub_box = [m.cx * k - hr, m.cy * k - hr, m.cx * k + hr, m.cy * k + hr]
    hub_col = hx(sc.hub)
    if m.hub_w:
        d.ellipse(hub_box, outline=hub_col, width=max(2, round(m.hub_w * k)))
    else:
        d.ellipse(hub_box, fill=hub_col)
    nr = m.node_r * k
    for i, (px, py) in enumerate(m.vertices):
        alert = i == m.alert_node
        if alert or not sc.dark:
            d.ellipse([px * k - nr, py * k - nr, px * k + nr, py * k + nr],
                      fill=hx(AMBER if alert else sc.node),
                      outline=hx("#FFD79A" if alert else sc.node_stroke),
                      width=max(2, round(4 * k)))
        else:
            sprite = _node_sprite(max(2, round(nr)), CYAN, sc.node)
            img.alpha_composite(sprite, (round(px * k - sprite.width / 2),
                                         round(py * k - sprite.height / 2)))
            d.ellipse([px * k - nr, py * k - nr, px * k + nr, py * k + nr],
                      outline=hx(sc.node_stroke), width=max(2, round(4 * k)))
    pulse = [(x * k, y * k) for x, y in m.pulse]
    if m.pulse_w and pulse:
        if sc.on_plate and m.pulse_cut and plate_img is not None:
            # 挖空 = 把同一张底板渐变贴回这条缝 ⇒ 缝色与渐变逐像素一致
            seam = Image.new("L", (canvas, canvas), 0)
            polyline(ImageDraw.Draw(seam), pulse, round(m.pulse_cut * k), 255)
            img.paste(plate_img, (0, 0), seam)
        # 脉冲在 SVG 里恒为水平渐变 url(#um-pulse),位图同样处理
        line = Image.new("L", (canvas, canvas), 0)
        polyline(ImageDraw.Draw(line), pulse, max(3, round(m.pulse_w * k)), 255)
        paint_gradient(img, line, _bbox_of(pulse), (1.0, 0.0),
                       [(0.0, MINT), (0.55, "#7BEBAE"), (1.0, MINT)])
    return img.resize((target, target), Image.LANCZOS)


def icon_params(target: int) -> MarkParams:
    """≥128px 完整版 → 33~127px 简化版 → ≤32px 极限版。"""
    if target >= 128:
        return FULL
    return COMPACT if target >= 33 else MICRO


def render_text(text: str, size: float, weight: int, track_em: float,
                colors: tuple, split_at: int = SPLIT_AT):
    """位图字标 → (图像, 图内基线 y, 排版宽, 墨迹左缘相对原点的偏移)。

    逐字形排版与 SVG 侧同一套 advance + tracking;返回偏移让调用方能把
    墨迹精确落到排版原点(而非把画布边距当成内容)。
    """
    px = max(8, round(size * SS))
    font = FACE.pil_font(weight, px)
    track = track_em * px
    adv = [font.getlength(c) for c in text]
    total = sum(adv) + track * (len(text) - 1)
    asc, desc = font.getmetrics()
    margin = px

    probe = Image.new("L", (math.ceil(total) + 2 * margin, asc + desc + 2 * margin), 0)
    ImageDraw.Draw(probe).text((margin, margin), text, font=font, fill=255)
    left, top, right, bottom = probe.getbbox()

    out = Image.new("RGBA", (math.ceil(total) + 2 * margin, asc + desc + 2 * margin),
                    (0, 0, 0, 0))
    d = ImageDraw.Draw(out)
    x = float(margin)
    for i, (ch, w) in enumerate(zip(text, adv)):
        col = hx(colors[0] if i < split_at else colors[1])
        d.text((x, margin), ch, font=font, fill=col)
        x += w + track
    out = out.crop((left, top, right, bottom)).resize(
        (max(1, (right - left) // SS), max(1, (bottom - top) // SS)), Image.LANCZOS)
    return out, (margin + asc - top) / SS, total / SS, (left - margin) / SS


def render_lockup(mark_h: int, inverse: bool = False, subtitle: bool = True) -> Image.Image:
    """横向锁定版位图:尺寸全部取自 lockup_metrics,与 SVG 同一套比例。"""
    k = lockup_metrics(mark_h)
    main_c = (SNOW, ICE) if inverse else (INK, BLUE)
    sub_c = ICE if inverse else SLATE
    mark = render_mark(mark_h, icon_params(mark_h),
                       sc=SCHEME_PLATE if not inverse else SCHEME_DARK,
                       plate=not inverse)
    wm, wm_base, _wm_w, wm_off = render_text(WORD_MAIN, k["main_size"], W_MAIN, TRACK_MAIN, main_c)
    sub = None
    if subtitle:
        sub, sub_base, _sw, sub_off = render_text(SUB_TEXT, k["sub_size"], W_SUB, TRACK_SUB,
                                                  (sub_c, sub_c), 99)
    pad = round(k["pad"])
    W, H = round(k["total_w"]), round(k["total_h"])
    out = Image.new("RGBA", (W, H), (0, 0, 0, 0))
    out.alpha_composite(mark, (pad, (H - mark_h) // 2))
    # 落位:图像像素 0 对应排版坐标 +off,故绝对位置 = 原点 + off;
    # 纵向按基线对齐:像素 0 在基线上方 base 处。
    out.alpha_composite(wm, (round(k["x_text"] + wm_off), round(k["base_main"] - wm_base)))
    if sub is not None:
        out.alpha_composite(sub, (round(k["x_text"] + sub_off),
                                  round(k["base_sub"] - sub_base)))
    return out


def render_og(path: str) -> None:
    """1584×704 社交分享图(2× 渲染后下采样)。"""
    W, H = 1584 * 2, 704 * 2
    img = gradient(W, H, 0, "#081726", "#143453", vec=(0.55, 1)).convert("RGBA")
    deco = render_mark(1520, FULL, sc=SCHEME_DARK, plate=False)
    layer = Image.new("RGBA", img.size, (0, 0, 0, 0))
    layer.alpha_composite(deco, (W - 1600, H - 1700))
    layer.putalpha(layer.getchannel("A").point(lambda v: int(v * 0.26)))
    img.alpha_composite(layer)

    lock = render_lockup(430, inverse=True)
    img.alpha_composite(lock, (170, 150))

    d = ImageDraw.Draw(img)
    bw, bh, gap, x0, y0 = 46, 150, 18, 190, 980
    seq = [MINT] * 11 + [AMBER] + [MINT] * 7 + ["#F56C6C"] * 2 + [MINT] * 12
    for i, c in enumerate(seq[:30]):
        x = x0 + i * (bw + gap)
        d.rounded_rectangle([x, y0, x + bw, y0 + bh], radius=14, fill=hx(c)[:3] + (235,))
    d.text((x0 + 6, y0 + bh + 60),
           "Multi-node probes  ·  round-based judgement  ·  one SQLite file",
           font=FACE.pil_font(W_SUB, 62), fill=hx(DIM))
    img.resize((1584, 704), Image.LANCZOS).save(path)


def render_sheet(path: str) -> None:
    """交付总览:主锁定版正反面 + 裸图形 + 尺寸阶梯(验收一目了然)。"""
    S = 2
    W, H = 1240 * S, 1000 * S
    img = gradient(W, H, 24 * S, "#FBFCFE", "#EDF2F8", vec=(0.4, 1)).convert("RGBA")
    d = ImageDraw.Draw(img)
    cap = FACE.pil_font(W_SUB, 26 * S)

    # 1) 浅底主锁定版
    lock = render_lockup(170, inverse=False)
    img.alpha_composite(lock, (70 * S, 70 * S))
    d.text((72 * S, (70 + 170 + 60) * S), "Primary lockup", font=cap, fill=hx(SLATE))

    # 2) 深底反白
    box_w, box_h = lock.width + 60 * S, lock.height + 60 * S
    dark = gradient(box_w, box_h, 30, "#0A1A2C", "#13324F", vec=(0.4, 1)).convert("RGBA")
    dark.alpha_composite(render_lockup(170, inverse=True), (30 * S, 30 * S))
    img.alpha_composite(dark, (70 * S, 330 * S))
    d.text((72 * S, (330 + 170 + 120) * S), "Inverse lockup", font=cap, fill=hx(SLATE))

    # 3) 裸图形:浅底 / 深底
    img.alpha_composite(render_mark(230, FULL, sc=SCHEME_LIGHT, plate=False),
                        (890 * S, 90 * S))
    img.alpha_composite(render_mark(230, FULL, sc=SCHEME_DARK, plate=False),
                        (890 * S, 350 * S))
    d.text((892 * S, 610 * S), "Bare mark, light / dark", font=cap, fill=hx(SLATE))

    # 4) 尺寸阶梯
    x, y = 70 * S, 700 * S
    for s in (128, 64, 48, 32, 24, 16):
        img.alpha_composite(render_mark(s, icon_params(s), sc=SCHEME_PLATE),
                            (x, y + (128 - s) // 2 * S))
        x += (s + 26) * S
    d.text((70 * S, (700 + 170) * S),
           "Size ramp - full >=128, simplified 33-127, minimal <=32",
           font=cap, fill=hx(SLATE))
    img.resize((1240, 1000), Image.LANCZOS).save(path)


# ==========================================================================
# 自检:对比度 / 小尺寸可读性 / 锁定版光学平衡 / SVG 合法性
# ==========================================================================
def _rel_lum(c: str) -> float:
    r, g, b = (pow(int(c.lstrip("#")[i:i + 2], 16) / 255, 2.2) for i in (0, 2, 4))
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def _contrast(a: str, b: str) -> float:
    la, lb = _rel_lum(a), _rel_lum(b)
    return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)


def check() -> list:
    problems: list[str] = []

    for name, fg, bgc, need in (
        ("hub/深底", SCHEME_DARK.hub, NAVY_TOP, 4.5),
        ("hub/浅底", SCHEME_LIGHT.hub, "#FFFFFF", 4.5),
        ("link/浅底", BLUE, "#FFFFFF", 2.2),
        ("link/深底", BLUE, NAVY_TOP, 2.2),
        ("字标主/浅底", INK, "#FFFFFF", 4.5),
        ("字标主/深底", SNOW, NAVY_TOP, 4.5),
        ("副标/浅底", SLATE, "#FFFFFF", 3.0),
        ("副标/深底", ICE, NAVY_TOP, 3.0),
        ("脉冲/深底", MINT, NAVY_TOP, 3.0),
        ("琥珀/深底", AMBER, NAVY_TOP, 3.0),
    ):
        c = _contrast(fg, bgc)
        if c < need:
            problems.append(f"对比度不足 {name}: {c:.2f} < {need}")

    # 小尺寸:每档都要"有暗底 + 有亮笔画",且笔画 ≥1 物理像素
    for size in (16, 24, 32, 48, 64):
        m = icon_params(size)
        a = np.asarray(render_mark(size, m, sc=SCHEME_PLATE))
        alpha = a[..., 3]
        lum = a[..., :3].astype(int).sum(axis=2)
        painted = alpha > 40
        ink = float(painted.mean())
        if not 0.30 < ink < 0.95:
            problems.append(f"{size}px 覆盖率异常: {ink:.2f}")
        if lum[painted].min() > 150:
            problems.append(f"{size}px 缺少深色底板像素")
        if lum[painted].max() < 600:
            problems.append(f"{size}px 亮笔画被抗锯齿抹平")
        for label, w in (("ring", m.ring_w), ("hub", m.hub_w or m.hub_r * 2),
                         ("node", m.node_r)):
            if w and w * size / m.size < 1.0:
                problems.append(f"{size}px 下 {label} 不足 1px({w * size / m.size:.2f}px)")

    # 锁定版光学平衡
    k = lockup_metrics()
    ratio = k["total_w"] / k["total_h"]
    if not 2.6 < ratio < 4.6:
        problems.append(f"锁定版长宽比失衡: {ratio:.2f}")
    if not 0.33 <= k["cap"] / k["mark"] <= 0.50:
        problems.append(f"字标与图形光学重量不匹配: cap/mark={k['cap'] / k['mark']:.2f}")
    first_glyph = k["x_text"] + min(off for _g, off, _a in k["main_placed"])
    if first_glyph < k["pad"] + k["mark"] + k["gap"] * 0.6:
        problems.append("字标离图形太近(安全间距不足)")

    # SVG:合法 XML + 无科学计数法坐标 + 锁定版必须嵌入字标 path
    sci = re.compile(r'd="[^"]*\d+e[+-]?\d', re.I)
    for f, raw, expect_paths in (
        ("uptimemesh-mark.svg", bare_svg(FULL), 0),
        ("uptimemesh-mark-badge.svg", badge_svg(FULL), 0),
        ("uptimemesh-logo-horizontal.svg", lockup_svg(False), 30),
        ("uptimemesh-logo-horizontal-inverse.svg", lockup_svg(True), 30),
    ):
        try:
            ET.fromstring(raw)
        except ET.ParseError as exc:
            problems.append(f"{f} 不是合法 XML: {exc}")
        if sci.search(raw):
            problems.append(f"{f} 含科学计数法坐标(部分渲染器不支持)")
        if raw.count("<path ") < expect_paths:
            problems.append(f"{f} 字标 path 只有 {raw.count('<path ')} 个,字形可能没嵌入")

    # 脉冲必须横贯图形(左右都探出六边形)
    xs = [p[0] for p in FULL.pulse]
    vx = [v[0] for v in FULL.vertices]
    if min(xs) > min(vx) or max(xs) < max(vx):
        problems.append("脉冲没有穿出网格边界,层级关系不成立")

    return problems


# ==========================================================================
def write(name: str, text: str) -> str:
    p = os.path.join(HERE, name)
    with open(p, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(text)
    return p


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", choices=["svg", "png", "all"], default="all")
    ap.add_argument("--check", action="store_true", help="只跑自检,不产出文件")
    args = ap.parse_args()

    if args.check:
        problems = check()
        for msg in problems:
            print("FAIL", msg)
        print("check:", "OK" if not problems else f"{len(problems)} problem(s)")
        raise SystemExit(1 if problems else 0)

    opts = args.only
    outs = []

    if opts in ("svg", "all"):
        outs += [
            write("uptimemesh-mark.svg", bare_svg(FULL, SCHEME_LIGHT)),
            write("uptimemesh-mark-on-dark.svg", bare_svg(FULL, SCHEME_DARK)),
            write("uptimemesh-mark-compact.svg", bare_svg(COMPACT, SCHEME_LIGHT)),
            write("uptimemesh-mark-badge.svg", badge_svg(FULL)),
            write("uptimemesh-mark-badge-compact.svg", badge_svg(COMPACT)),
            write("uptimemesh-logo-horizontal.svg", lockup_svg(False)),
            write("uptimemesh-logo-horizontal-inverse.svg", lockup_svg(True)),
        ]
    if opts in ("png", "all"):
        for size in (512, 256, 128, 64, 48, 32, 24, 16):
            p = os.path.join(HERE, f"uptimemesh-icon-{size}.png")
            render_mark(size, icon_params(size), sc=SCHEME_PLATE).save(p)
            outs.append(p)
        ico = os.path.join(HERE, "uptimemesh-favicon.ico")
        # 每档按自己的尺寸重新渲染(≤32px 走 MICRO 几何),而不是缩一张图
        frames = [render_mark(s, icon_params(s), sc=SCHEME_PLATE) for s in (16, 24, 32, 48)]
        frames[-1].save(ico, format="ICO", sizes=[f.size for f in frames],
                        append_images=frames[:-1])
        outs.append(ico)
        for name, sc, bg in (
            ("uptimemesh-mark-on-light.png", SCHEME_LIGHT, (248, 250, 252, 255)),
            ("uptimemesh-mark-on-dark.png", SCHEME_DARK, (11, 27, 45, 255)),
        ):
            p = os.path.join(HERE, name)
            render_mark(256, FULL, sc=sc, plate=False, bg=bg).save(p)
            outs.append(p)
        for name, inv in (("uptimemesh-logo-horizontal.png", False),
                          ("uptimemesh-logo-horizontal-inverse.png", True)):
            p = os.path.join(HERE, name)
            render_lockup(200, inverse=inv).save(p)
            outs.append(p)
        for name, fn in (("uptimemesh-og-banner.png", render_og),
                         ("uptimemesh-brand-sheet.png", render_sheet)):
            p = os.path.join(HERE, name)
            fn(p)
            outs.append(p)

    for p in outs:
        print(f"{os.path.basename(p):42s} {os.path.getsize(p):>9,d} bytes")


if __name__ == "__main__":
    main()
