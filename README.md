# UptimeMesh

<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)"
            srcset="docs/logo/uptimemesh-logo-horizontal-inverse.svg">
    <img alt="UptimeMesh" src="docs/logo/uptimemesh-logo-horizontal.svg" width="520" height="142">
  </picture>
</div>

**分布式主动监控系统** —— Dashboard 只负责配置、调度与汇总，检测全部由分布在各处的 Agent（节点）执行。

**中文** | [English](README.en.md)

---

## 这个项目和 Uptime Kuma 是什么关系？

UptimeMesh 的灵感来自 [Uptime Kuma](https://github.com/louislam/uptime-kuma/)：监控/通知的交互形态、通知模板与占位符、HTTP 关键词与 JSON 查询断言、TCP 端口、外部上报（Push）等概念都明显沿用了它的设计，并且内置了从 UptimeKuma 一键导入监控配置的能力。这里向 Uptime Kuma 及其作者致敬。

> [!IMPORTANT]
> **如果只需要做常规监控、不需要多节点从多个位置探测，强烈建议直接使用 [Uptime Kuma](https://github.com/louislam/uptime-kuma/)。**
> 它更加全面、稳定、好用：生态与通知渠道更丰富、界面与运维更成熟、社区活跃、文档完善，单机部署也有大量现成的经验可以借鉴。
> UptimeMesh 存在的唯一理由是「**同一次探测要在多个不同网络位置的节点上分别执行，并按轮次聚合判定**」——除此之外的场景，Uptime Kuma 都是更好的选择。

那什么时候才轮到 UptimeMesh？

| 你的诉求 | 推荐 |
|---|---|
| 单机部署、常规 URL/PING/端口/推送监控、丰富通知渠道 | **Uptime Kuma**（更成熟全面） |
| 想少运维、开箱即用、社区插件/主题/多语言 | **Uptime Kuma** |
| 需要从多地节点分别探测同一目标，按轮次聚合成功率判定 | UptimeMesh |
| 需要节点自报网络能力（IPv4/IPv6）、按地域展示节点 | UptimeMesh |
| 需要在仪表盘内一键安装/审批/升级自建节点（systemd 服务） | UptimeMesh |
| 需要内网机器主动连出（无需被监控侧开放端口）的多点探测 | UptimeMesh |

## 特性

- **多节点探测**：一个监控可指派给多个 Agent（指定节点 / 所有节点 / 排除节点三种模式），各节点独立探测同一次轮次，Dashboard 按轮次聚合出「本次成功率」。
- **五种监控类型**：HTTP(S)、PING（ICMP）、TCP 端口、外部上报（Push）、下载速度。
- **轮次驱动的判定与告警**：本次成功率低于阈值且**连续 N 轮**才判 DOWN，翻转时推送告警，恢复时推送通知；状态变动有独立记录与时间线。
- **缺样有明确口径**：节点离线不计入分母、节点活着没回传记为失败、任务从未派发单独留痕（不把在线节点谎报成「离线」）。
- **通知能力**：通用 JSON Webhook，支持按事件类型（DOWN / UP / TEST）自定义标题与正文模板；每个渠道还可填写**请求体模板**，直接适配钉钉 / 企业微信 / Slack / bark 等机器人要求的 JSON 结构，渠道页支持「测试发送」与「设置为所有监控的渠道」。
- **节点全生命周期**：一键安装脚本（Linux + systemd，内置分发二进制）、首次接入待审批、专属凭据持久化、吊销 / 删除（伪删除，历史记录保留）、**一键自升级**与「版本落后 / 需重装」提示。
- **探测细节可控**：期望状态码（含 `400~499` 区间）、包含 / 禁止关键字、JSON 查询断言（JSONata）、忽略证书校验、跟随重定向、IP 协议族（自动 / IPv4 / IPv6）、**反转模式**（探测失败算正常）。
- **组织与展示**：监控分组、总览页分组卡片聚合、最近状态色块、趋势图、分节点轮次明细、地域（国旗 + 本地化名称）与节点网络能力标签。
- **存储省心**：SQLite 单文件（纯 Go 驱动，无 CGO），无需额外数据库容器；原始结果按保留期（默认 31 天）清理，小时级聚合（24h/7d/30d 可用率的数据源）固定保留 31 天；设置页可查看数据库占用并「压缩数据库」。
- **配置迁移**：设置页可把监控（五种类型与全部探测细节）、通知渠道、通知模板与面板设置**导出成一份 JSON 文件**，也能把这样一份文件导入进来（换机迁移 / 批量备份）；文件不含密钥与历史数据，**旧版本文件可直接导入，更高版本的文件会被拒绝**并提示先升级仪表盘。
- **前端中英双语**：默认中文，右上角可切换英文（登录页也可切换）。

## 架构与工作原理

```text
                    ┌─────────────────────────────────────────┐
   浏览器  ──REST──▶│  Dashboard（GoFrame）                   │
   (Vue 3)  ◀─WS────│  · 监控 / 节点 / 渠道 / 设置            │
                    │  · 轮次调度：建轮 → 下发 → 收口定稿     │
                    │  · 聚合统计 + 告警状态机 + Webhook 投递 │
                    │  · SQLite 单文件存储                    │
                    └───────────────▲─────────────────────────┘
                                    │ WebSocket 长连接（Agent 主动连入）
                    ┌───────────────┴──┬──────────────────┐
                    │ Agent 北京-01    │ Agent 香港-01     │ … 任意多个节点
                    │ HTTP / PING / TCP / 下载测速        │
                    └──────────────────┴──────────────────┘
```

三条核心原则（决策记录见 [`docs/adr/`](docs/adr/)）：

1. **Dashboard 永不执行检测**（[ADR-0002](docs/adr/0002-dashboard-never-checks.md)）：可用性只以节点上报为准。
2. **轮次由 Dashboard 驱动**（[ADR-0003](docs/adr/0003-dashboard-driven-probe-rounds.md)）：按监控周期创建轮次快照并下发，多节点结果按轮次聚合；整轮无样本 ⇒ UNKNOWN。
3. **WebSocket 长连接**（[ADR-0001](docs/adr/0001-websocket-command-channel.md)）：Agent 主动连入 Dashboard，实时下达任务与回传结果；心跳 10s、丢失 3 次判离线，另有周期保活探活。

存储为 SQLite 单文件（[ADR-0005](docs/adr/0005-sqlite-storage.md)，取代早期的 MongoDB 方案 [ADR-0004](docs/adr/0004-official-mongo-driver.md)）；数据库压缩策略见 [ADR-0006](docs/adr/0006-db-compaction-vacuum-only.md)。

## 快速开始

前置条件：Docker 与 Docker Compose。

### 1. 启动 Dashboard

```bash
cd deploy
docker compose up -d --build
```

- 访问地址：`http://localhost:5678`（`deploy/docker-compose.yml` 把宿主 `5678` 映射到容器 `8000`，想换端口改这里的 `ports` 即可）
- API 前缀 `/api/v1/*`，前端页面在根路径；健康检查 `GET /api/v1/health`
- 数据存放在命名卷 `uptimemesh-data` 的 `/data/uptimemesh.db`（SQLite 单文件，**不需要**单独的数据库容器）

### 2. 初始化管理员

浏览器打开 `http://localhost:5678`，首次进入是初始化页，设置管理员账号与密码（密码 ≥ 6 位）后即可登录。

### 3. 取得接入密钥

Dashboard 容器**首次启动**时会在日志里打印初始接入密钥：

```bash
docker logs uptimemesh-dashboard 2>&1 | grep -A1 "初始接入密钥"
```

也可以登录后直接在「设置」页查看并复制当前密钥（明文随密钥一起保存），随时可轮换（旧密钥立即失效，已批准节点的专属凭据不受影响）。「节点」页的「复制安装代码」弹窗会自动填入该密钥。

### 4. 部署第一个节点（Agent）

**方式 A（推荐）：节点页一键安装。** 打开「节点」页 → 顶部「复制安装代码」（弹窗已自动填好接入密钥，按需改节点名）→ 复制命令，在目标 Linux 机器上以 root 执行：

```bash
curl -fsSL http://<dashboard地址>:5678/api/v1/agent/install.sh | sudo sh -s -- \
  --server ws://<dashboard地址>:5678/ws/agent --key <接入密钥> --name 北京-阿里云-01
```

脚本按 `uname -m` 自动选择 linux/amd64 或 linux/arm64 安装包（由 Dashboard 内置分发），安装到 `/usr/local/bin/uptimemesh-agent`，写入 systemd 单元（开机自启、崩溃重启、`CAP_NET_RAW` 以支持 ICMP PING），密钥存放于 `/etc/uptimemesh-agent/env`（仅 root 可读），凭据持久化在 `/var/lib/uptimemesh-agent`。执行完成后回到「节点」页点「批准」即可。

**方式 B：手动运行二进制。**

```bash
# 仓库内构建：./deploy/build-agent.sh（默认交叉编译 linux/amd64 与 linux/arm64 到 bin/）
./bin/uptimemesh-agent-linux-amd64 \
  --server ws://<dashboard地址>:5678/ws/agent \
  --key <接入密钥> \
  --name 北京-阿里云-01 \
  --cred-dir /var/lib/uptimemesh-agent
```

**方式 C：Docker 运行 Agent。**

```bash
docker build -f deploy/Dockerfile.agent -t uptimemesh-agent .
docker run -d --name uptimemesh-agent --restart unless-stopped \
  --network host --cap-add NET_RAW \
  -v uptimemesh-agent-cred:/data \
  uptimemesh-agent \
  --server ws://<dashboard地址>:5678/ws/agent \
  --key <接入密钥> --name 机房B --cred-dir /data
```

节点首次连接进入「待审批」，在「节点」页点「批准」后签发专属凭据（持久化到 `--cred-dir`，此后重连免审批）。节点退役时先在节点页「吊销」（凭据立即失效），再「删除」；删除是伪删除——列表不再显示，历史检测记录仍保留节点名，若该机器再次接入会作为新节点重新进入待审批。

> 一键安装（方式 A）依赖 Dashboard 内置的节点二进制：Docker 部署时随镜像自动交叉编译；本地非容器运行 Dashboard 时，先执行 `./deploy/build-agent.sh` 生成 `bin/uptimemesh-agent-linux-*`，Dashboard 会自动从 `bin/` 分发。

### 5. 创建监控并等待告警

「监控」→「新建监控」：

| 配置 | 说明 |
|---|---|
| 类型 | HTTP(S)：方法 / 请求头 / 请求体 / 期望状态码 / 关键字 / JSON 查询断言；PING：ICMP 单包（每节点每轮 1 包）；TCP 端口：建连成功即成功；外部上报：由外部系统请求专属地址上报；下载速度：各节点下载文件测速 |
| 周期 | 10~3600 秒（轮次频率，默认 60）；下载速度监控为监控内逐节点串行测速，实际间隔不小于周期 |
| 超时 | 探测超时，须 ≤ 周期 |
| 阈值 + 连续低于阈值轮数 | 默认 `100% × 3 轮`：最近连续 3 轮「本次成功率」低于 100% 才判 DOWN 并告警（下载速度监控的阈值是速度下限，单位可选 `KB/s` / `MB/s`） |
| 指派节点 | 至少一个**已批准**节点，可选「所有节点（含后续新增）」或「排除节点」，新增节点后无需再编辑 |
| 通知渠道 | 先在「通知渠道」页创建全局 Webhook，再在监控里勾选使用 |

保存后会立刻补一轮检测（不等满一个周期），暂停后恢复同理。「测试」按钮可以把**尚未保存**的配置交给一个在线节点跑一次，并展示实际请求与响应明细，不建轮次、不落库。

### 6. 配置通知渠道

「通知渠道」页新建 Webhook（`http(s)` 地址）：

- 留空请求体模板 ⇒ 发送通用 JSON：结构化字段（`event` / `monitorName` / `url` / `roundSuccessRate` / `errorCount` / `timestamp` 等）加上按模板渲染好的 `title` / `content`。
- 填写**请求体模板** ⇒ 直接适配钉钉 / 企业微信 / Slack 等机器人要求的 JSON 结构，支持 `{{title}}`、`{{content}}` 及各事件变量，变量值按 JSON 规则转义，渲染后必须是合法 JSON（弹窗内有变量说明与实时预览）。
- 文案在「设置 → 通知模板」按事件类型（DOWN / UP / TEST）分别配置，支持 `{{monitorName}}`、`{{url}}`、`{{successRate}}`、`{{speed}}`、`{{agents}}`、`{{errorCount}}`、`{{timestamp}}` 等占位符；`{{agents}}` 是触发翻转那一轮的**节点明细**（每个节点一行），`{{errorCount}}` 是当前仍处于报警状态的启用中监控数。清空即恢复内置默认模板。
- 渠道页支持「测试发送」与「设置为所有监控的渠道」（追加到所有监控，不影响其它渠道）。
- 每个渠道带**启用开关**（列表行与弹窗内都有）：禁用的渠道不接收告警与恢复通知，但监控里对它的勾选保留 —— 停用只是为了临时静默（比如群里在排查别的事），重新启用即刻恢复投递，不必再逐个改监控；「测试发送」不受启用状态限制，便于先验通 URL 与模板再启用。

## 本地开发

```bash
# Dashboard（无需任何外部数据库，自动创建 SQLite 库到 dashboard/data/uptimemesh.db）
cd dashboard && go run ./cmd        # 端口见 dashboard/manifest/config/config.yaml（默认 8000）

# 前端（Vite 开发服务器把 /api 与 /ws 代理到 127.0.0.1:5678）
cd web && npm install && npm run dev   # http://localhost:5173
```

> 测试代码（`*_test.go` 及测试辅助包）不入库，仅保留在本地开发环境，因此公开仓库里跑 `go test ./...` 不会有任何用例。

> 容器与本地可以共存：compose 用 `SERVER_PORT=8000` 让容器监听 8000 并把宿主 5678 映射过去，本地 `go run` 走 `config.yaml`。若希望本地 Dashboard 与前端默认代理对齐，用 `SERVER_PORT=5678 go run ./cmd`，或把 `web/vite.config.ts` 里的代理端口改成 8000。SQLite 库文件路径由 `SQLITE_PATH`（或 `config.yaml` 的 `sqlite.path`）指定。

> **本地跑看不到地域？** 节点页的地域靠本地 MaxMind 地域库解析，容器镜像已内置（构建时下载），本地 `go run` 没有。在**仓库根目录**执行 `sh deploy/fetch-geoip.sh` 下一份，再带路径启动即可；不指定时节点地域只能手动设置，页面会显示「未知」。
>
> ```bash
> cd dashboard && GEOIP_DB_PATH=../deploy/geoip/GeoLite2-Country.mmdb go run ./cmd
> ```
>
> 启动日志会打印 `已加载地域库: …（数据版本 YYYY-MM-DD）`；地域库按周更新，数据版本太旧时重新跑一次脚本即可。库由 MaxMind 制作（GeoLite2），公开镜像源见脚本内注释。

前端国际化改动请先跑一次静态检查：

```bash
cd web && npm run i18n:check   # 扫描硬编码中文；构建 npm run build 还会用 vue-tsc 校验中英词条一致性
```

## 构建与发布

- **Docker（推荐）**：`cd deploy && docker compose up -d --build`。`Dockerfile.dashboard` 会在镜像内先跑 `npm run build`，再把产物写入 `go:embed` 目录，因此每次 `--build` 都会带上最新前端，无需手工拷贝。
- **本地非容器**：`cd web && npm run build`，把 `web/dist/` 拷入 `dashboard/internal/webdist/dist/`，再 `cd dashboard && go build -o uptimemesh ./cmd`。注意 `go build` 只嵌入已存在的 `webdist/dist/`，改了前端务必先重新拷贝，否则仍会跑旧页面。
- **节点版本号**：`deploy/docker-compose.yml` 的 `AGENT_VERSION` 会被打进 Agent 二进制（`-ldflags -X …agentclient.Version`），同时写进分发目录的 `VERSION` 文件，节点页据此判断「可升级 / 需重装」，两者必须一致——用 `sh deploy/bump-agent-version.sh` 一并修改。

## 目录结构

```text
dashboard/        GoFrame 服务端（REST + /ws/agent + /ws/browser + 轮次调度 + 告警通知）
  internal/
    store/        SQLite 数据访问（纯 Go 驱动 + schema.sql）
    hub/          Agent 连接注册表与 WS 会话
    scheduler/    轮次创建 / 下发 / 收口定稿 / 缺样决策
    alert/        告警状态机（纯函数）
    notifier/     Webhook 投递（重试 5/15/30s）
    webhub/       浏览器实时推送
    retention/    过期结果清理
    api/ auth/ webdist/
agent/            节点进程（连接、执行探测、回传、凭据持久化、一键自升级）
shared/           协议与探测实现（agent 与 dashboard 共用）
web/              Vue 3 + Vite + TS + Element Plus + ECharts 前端
deploy/           docker-compose、Dockerfile、build-agent.sh、bump-agent-version.sh、.env.example
docs/             adr/（架构决策）、protocol.md（WS 帧契约）、logo/（品牌标识）、agents/（协作指南）
```

## 文档与贡献

- 领域术语与口径：[`CONTEXT.md`](CONTEXT.md)
- 架构决策：[`docs/adr/`](docs/adr/)
- Agent ↔ Dashboard 帧协议：[`docs/protocol.md`](docs/protocol.md)
- 协作约定（工单、标签、目录与提交规范）：[`AGENTS.md`](AGENTS.md)、[`docs/agents/`](docs/agents/)
- 前端新增任何面向用户的文本都必须国际化（`web/src/i18n/`，中英词条必须同时修改）。
- 任何影响 Agent 二进制的改动（`agent/`、`shared/` 的 Go 代码及构建方式）都要让版本修订号 +1。

## 品牌标识

Logo 全部由脚本从同一套几何 + 色板生成，改数值重跑即可，SVG 与 PNG 永不走偏：

```bash
python docs/logo/generate_logo.py            # 重新生成全部产物
python docs/logo/generate_logo.py --check    # 设计自检（对比度 / 小尺寸可读性 / 版式平衡 / SVG 合法性）
```

| 用途 | 文件 |
|---|---|
| README / 文档主锁定版（浅底、深底各一） | `docs/logo/uptimemesh-logo-horizontal{,-inverse}.svg` |
| 图形（无底板，深浅底通用） | `docs/logo/uptimemesh-mark.svg`、`uptimemesh-mark-on-dark.svg` |
| 头像 / 应用图标（带圆角底板） | `docs/logo/uptimemesh-mark-badge.svg`、`uptimemesh-icon-{512,256,128,64,48,32,24,16}.png` |
| favicon | `docs/logo/uptimemesh-favicon.ico`（16/24/32/48 多帧） |
| 社交分享图 | `docs/logo/uptimemesh-og-banner.png`（1584×704） |
| 交付总览（验收用） | `docs/logo/uptimemesh-brand-sheet.png` |

- **含义**：六边形是分散在各处的节点网格（Mesh），中心是 Dashboard，横贯的心电线是主动探测（Uptime），右下那颗琥珀色节点表示「正在报警」。
- **色板与仪表盘同源**：品牌蓝 `#2D7FF9` 提炼自 Element Plus primary `#409eff`，在线绿 `#3DDC84` 与报警琥珀 `#F5A623` 对应状态色 up / breach。
- **小尺寸另有专用几何**：≥128px 用完整版，33~127px 用简化版（加粗线条、去掉对角细线），≤32px 用极限版——直接缩大图会让中心白点与细线在抗锯齿下消失。
- 设计口径与使用规范见 [`docs/logo/README.md`](docs/logo/README.md)。

---

**中文** | [English](README.en.md)
