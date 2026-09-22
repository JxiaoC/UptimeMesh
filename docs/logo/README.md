# UptimeMesh 品牌标识

一句话说清楚：**所有 Logo 文件都是 `generate_logo.py` 从同一套几何 + 色板生成出来的**，
SVG（浏览器/文档用）与 PNG/ICO（位图用）永远不会走偏；要改样式，改脚本参数重跑。

```bash
python docs/logo/generate_logo.py            # 生成全部产物
python docs/logo/generate_logo.py --only svg # 只要矢量（调几何最快）
python docs/logo/generate_logo.py --check    # 设计自检，见下文「验收口径」
```

依赖：`pillow`、`numpy`、`fonttools`（`pip install pillow numpy fonttools`）。
字标取系统字体 Cascadia Code（SIL Open Font License 1.1），产物里已转成曲线，
**仓库与使用者都不需要安装该字体**；换字体用 `UM_LOGO_FONT=/path/to.ttf`。

---

## 标识讲了什么

```
        ●  ← 节点：分布在不同网络位置的 Agent
      ╱   ╲
    ●       ●
     │ ╲ ● ╱ │   ← 六边形 = Mesh（节点网格），中心 = Dashboard
    ●       ●
      ╲   ╱
        ◉  ← 琥珀色：这个节点正在报警
   ～～～╱╲～～～  ← 心电线 = Uptime（主动探测的生命体征）
```

| 元素 | 含义 | 对应项目事实 |
|---|---|---|
| 六边形 + 6 个节点 | 多节点从多地探测 | 一个监控可指派多个 Agent |
| 中心圆 | Dashboard | [ADR-0002](../adr/0002-dashboard-never-checks.md)：它永不执行检测，只收口与聚合 |
| 两条对角细线 | 节点与中心之间的长连接 | WebSocket 命令通道（[ADR-0001](../adr/0001-websocket-command-channel.md)） |
| 横贯的心电线 | 主动探测 | 轮次驱动的探测与成功率曲线（[ADR-0003](../adr/0003-dashboard-driven-probe-rounds.md)） |
| 右下琥珀色节点 | 正在报警 | 成功率跌破阈值、连续 N 轮翻转为 DOWN |

心电线**压在网格之上并挖出一条缝**（SVG 里用同一条底板渐变描边实现，
不是盖一块实色），所以读起来是"穿过"而不是"糊在上面"。

## 色板（与仪表盘同源）

| 变量 | 值 | 来源 / 用途 |
|---|---|---|
| `BLUE` | `#2D7FF9` | 由 Element Plus primary `#409eff` 提纯，mesh 连线主色 |
| `BLUE_DEEP` | `#1B5FD9` | 节点暗端 |
| `CYAN` | `#5CD3FF` | 节点高光、底板描边 |
| `MINT` | `#3DDC84` | 脉冲；对应 UI 的 up 态（`#67c23a` 提纯） |
| `AMBER` | `#F5A623` | 报警节点；对应 breach 态（`#e6a23c` 提纯） |
| `NAVY_TOP/BOT` | `#0B1E33` → `#173C5C` | 底板对角渐变（userSpaceOnUse） |
| `INK` / `SLATE` | `#0E2233` / `#53718C` | 浅底字标主色 / 副标语 |
| `SNOW` / `ICE` | `#F2F8FF` / `#8FC4FF` | 深底字标主色 / 次色 |

字标 `Uptime`＋`Mesh` 双色（浅底：墨色 + 品牌蓝；深底：白 + 浅蓝），
读法上就是"uptime"与"mesh"两段语义的拼接。

## 三套几何：为什么小尺寸要单独画

直接缩小大图会在 favicon 尺度上塌掉（实测：16px 下中心白点整体消失）。
`icon_params()` 按目标尺寸自动选档：

| 档位 | 尺寸 | 参数 | 做了哪些取舍 |
|---|---|---|---|
| `FULL` | ≥128px | 线宽 9 / 节点 21 / 有心电线 + 对角细线 | 细节完整 |
| `COMPACT` | 33~127px | 线宽 16 / 节点 30 / 去掉对角细线 | 牺牲"长连接"暗示，保住网格与脉冲 |
| `MICRO` | ≤32px | 线宽 34 / 节点 36 / 中心改实心 / **不画脉冲** | 只保"网格 + 中心 + 报警点"，脉冲在此尺度只会糊成灰带 |

`uptimemesh-favicon.ico` 的 16/24/32/48 四帧**各按自己的尺寸重新渲染**，
不是把一张图缩下去。

## 文件清单

| 文件 | 说明 |
|---|---|
| `uptimemesh-logo-horizontal.svg` / `-inverse.svg` | 横向主锁定版（浅底 / 深底），README、文档首页用 |
| `uptimemesh-logo-horizontal.png` / `-inverse.png` | 同上位图版（不支持 `<picture>` 的场合） |
| `uptimemesh-mark.svg` | 裸图形（浅底通用），深浅底皆可 |
| `uptimemesh-mark-on-dark.svg` | 裸图形深底版（无底板，直接放深色背景上） |
| `uptimemesh-mark-compact.svg` | 裸图形简化版 |
| `uptimemesh-mark-badge.svg` / `-compact.svg` | 带圆角方底板：头像、包图标 |
| `uptimemesh-icon-{512,256,128,64,48,32,24,16}.png` | 图标尺寸阶梯 |
| `uptimemesh-favicon.ico` | 多帧 favicon |
| `uptimemesh-mark-on-{light,dark}.png` | 裸图形在两种背景下的预览 |
| `uptimemesh-og-banner.png` | 1584×704 社交分享图 |
| `uptimemesh-brand-sheet.png` | 交付总览图（一次看全主锁定版正反面、裸图形、尺寸阶梯） |
| `generate_logo.py` | 唯一事实源 |

## 使用规范

- **留白**：四周不小于图形高度的 15%（脚本里 `LOCK_PAD_RATIO`）；不要把字标压到图形上。
- **最小尺寸**：锁定版宽不小于 120px；图形不小于 16px（且必须用 `-compact`/`MICRO` 档产物）。
- **深浅底**：只允许用对应的两版（浅底 `INK`、深底 `SNOW`）。不要给锁定版另加背景色块，
  底板只在 `badge` 变体里出现。
- **不要**拉伸变形、旋转、改色、加投影或描边；需要新尺寸请改脚本参数重跑，不要在位图上缩放。
- 单色场景（雕刻、印刷）用 `uptimemesh-mark.svg` 手动改成纯 `#000`/`#FFF`，
  并把脉冲与网格的挖空缝去掉。

## 验收口径（`--check` 在验什么）

改完参数先跑 `--check`，它会挡住这类退化：

1. **对比度**：10 组前后景（字标、连线、枢纽、脉冲、琥珀…）按 WCAG 相对亮度比给最低值。
2. **小尺寸可读性**：16/24/32/48/64px 逐档渲染，检查覆盖率区间、"必须有深色底板 + 有亮笔画"，
   并把线宽折算成物理像素（`<1px` 直接判失败）。
3. **锁定版光学平衡**：长宽比区间、`cap高/图形边长` 区间（文字与图形是否等重）、字标安全间距。
4. **SVG 合法性**：XML 可解析、path 坐标不含科学计数法（部分渲染器不支持）、
   锁定版必须真的嵌入了字标 path。
5. **层级关系**：脉冲两端必须探出六边形顶点，否则"穿过网格"不成立。

另外做过一次跨渲染器核对（一次性验证，不在仓库脚本里）：把 SVG 用无头 Chrome 栅格化，
与 Pillow 产物在 512px 下逐像素比，主图形平均色差 5.4/255、锁定版 7.4/255，
属抗锯齿噪声级；过程中据此修掉过三个真实缺陷——底板渐变方向与 SVG 不一致
（`t` 应是点到向量的投影，不是两轴加权和）、位图漏画底板青色描边、
位图把网格/脉冲画成实色而 SVG 用的是渐变（`paint_gradient()` 现按
objectBoundingBox 语义刷同一支渐变）。位图与 SVG 因此不是"各画一遍碰运气"。
