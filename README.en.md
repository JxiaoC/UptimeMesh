# UptimeMesh

<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)"
            srcset="docs/logo/uptimemesh-logo-horizontal-inverse.svg">
    <img alt="UptimeMesh" src="docs/logo/uptimemesh-logo-horizontal.svg" width="520" height="142">
  </picture>
</div>

**A distributed active monitoring system** — the Dashboard only stores configuration, schedules probe rounds and aggregates results; **every check runs on Agents (nodes) deployed wherever you need them**.

[中文](README.md) | **English**

---

## How does this project relate to Uptime Kuma?

UptimeMesh is inspired by [Uptime Kuma](https://github.com/louislam/uptime-kuma/): its monitor/notification interaction model, notification templates and placeholders, HTTP keyword checks and JSON query assertions, TCP port monitors and Push (external report) monitors all clearly follow Kuma's design, and UptimeMesh ships a one-click importer for existing UptimeKuma monitor configuration. Credit and thanks go to Uptime Kuma and its author.

> [!IMPORTANT]
> **If you only need regular monitoring and do not need multi-node probing from several locations, please use [Uptime Kuma](https://github.com/louislam/uptime-kuma/) directly.**
> It is more complete, more stable and nicer to operate: richer integrations and notification channels, a more mature UI and operational story, an active community and thorough documentation, plus plenty of field-proven single-host deployment experience.
> The only reason UptimeMesh exists is the case where **the same probe must run from several nodes in different network locations and be judged on a per-round aggregate** — for everything else, Uptime Kuma is the better choice.

So when is UptimeMesh the right tool?

| What you need | Recommendation |
|---|---|
| Single-host deployment, regular URL/PING/port/push monitoring, rich notification channels | **Uptime Kuma** (more mature and complete) |
| Minimal operations, works out of the box, community plugins/themes/locales | **Uptime Kuma** |
| Probing the same target from several locations and judging on a per-round success rate | UptimeMesh |
| Nodes reporting their own network capability (IPv4/IPv6) and being grouped by region | UptimeMesh |
| Installing, approving and upgrading your own nodes (systemd services) from the dashboard | UptimeMesh |
| Multi-point probing from inside private networks (nodes dial out, no inbound port needed) | UptimeMesh |

## Features

- **Multi-node probing**: a monitor can be assigned to several Agents (fixed node list / all nodes / all-but-excluded); every node probes the same round independently, and the Dashboard aggregates this round's success rate.
- **Five monitor types**: HTTP(S), PING (ICMP), TCP port, Push (external report) and Download speed.
- **Round-driven judging and alerting**: a monitor flips to DOWN only when this round's success rate stays below the threshold for **N consecutive rounds**; flips send alerts and recoveries send notifications; every state change is recorded with its round and shown on a timeline.
- **Explicit missing-sample semantics**: offline nodes are excluded from the denominator, nodes that are alive but silent count as failures, and tasks that were never dispatched are recorded separately (an online node is never falsely reported as “offline”).
- **Notifications**: a generic JSON webhook with per-event (DOWN / UP / TEST) title and body templates; each channel can also define a **request body template** so the payload matches what DingTalk / WeCom / Slack / bark bots expect. Channels support “test send” and “apply to all monitors”.
- **Full Agent lifecycle**: a one-line installer (Linux + systemd, with binaries distributed by the Dashboard), pending-approval enrollment, persisted per-node credentials, revoke / delete (soft delete, history kept), **one-click self-upgrade** and “outdated / needs reinstall” hints.
- **Fine-grained probe control**: expected status codes (including ranges such as `400~499`), required / forbidden keywords, JSONata JSON assertions, ignore TLS errors, redirect following, IP version (auto / IPv4 / IPv6) and **invert mode** (a failed probe counts as healthy).
- **Organisation and presentation**: monitor groups, group cards on the overview page, recent-status strips, trend charts, per-node round details, region (flag + localized name) and node network-capability badges.
- **Low-maintenance storage**: a single SQLite file (pure-Go driver, no CGO) with no separate database container; raw results are pruned by retention (31 days by default) and hourly aggregates (the data source for the 24h/7d/30d availability rates) are kept for a fixed 31 days; the settings page shows database usage and offers a “compact database” action.
- **Configuration migration**: the settings page exports monitors (all five types with every probe detail), notification channels, notification templates and panel settings into **one JSON file**, and imports such a file back (moving to another machine, bulk backups). The file holds no secrets and no history, **older files import directly while a newer file is rejected** with a prompt to upgrade the dashboard first.
- **Bilingual UI**: Chinese by default, switchable to English from the top-right corner (also on the login page).

## Architecture and how it works

```text
                    ┌─────────────────────────────────────────┐
   Browser  ──REST─▶│  Dashboard (GoFrame)                    │
   (Vue 3)  ◀──WS───│  · monitors / agents / channels / config│
                    │  · round scheduler: create → dispatch → │
                    │    finalize                             │
                    │  · aggregation + alert state machine +  │
                    │    webhook delivery                     │
                    │  · single-file SQLite storage           │
                    └───────────────▲─────────────────────────┘
                                    │ WebSocket (agents dial in)
                    ┌───────────────┴──┬──────────────────┐
                    │ Agent beijing-01 │ Agent hongkong-01│ … as many nodes as you need
                    │ HTTP / PING / TCP / download speed   │
                    └──────────────────┴──────────────────┘
```

Three core principles (see the decision records in [`docs/adr/`](docs/adr/)):

1. **The Dashboard never performs checks** ([ADR-0002](docs/adr/0002-dashboard-never-checks.md)): availability is based solely on what nodes report.
2. **Rounds are driven by the Dashboard** ([ADR-0003](docs/adr/0003-dashboard-driven-probe-rounds.md)): a round snapshot is created per monitor period and dispatched, and multi-node results are aggregated per round; a round with no samples at all becomes UNKNOWN.
3. **A WebSocket long connection** ([ADR-0001](docs/adr/0001-websocket-command-channel.md)): Agents dial into the Dashboard, so tasks and results flow in real time; heartbeats run every 10 s and three missed beats mark a node offline, with an additional periodic keepalive probe.

Storage is a single SQLite file ([ADR-0005](docs/adr/0005-sqlite-storage.md), which superseded the earlier MongoDB approach [ADR-0004](docs/adr/0004-official-mongo-driver.md)); the compaction policy is described in [ADR-0006](docs/adr/0006-db-compaction-vacuum-only.md).

## Quick start

Prerequisite: Docker and Docker Compose.

### 1. Start the Dashboard

```bash
cd deploy
docker compose up -d --build
```

- URL: `http://localhost:5678` (`deploy/docker-compose.yml` maps host port `5678` to container port `8000`; change `ports` there to use another port)
- API prefix `/api/v1/*`, the web UI is served at the root path; health check: `GET /api/v1/health`
- Data lives in the named volume `uptimemesh-data` at `/data/uptimemesh.db` (single SQLite file; **no** separate database container)

### 2. Initialize the administrator

Open `http://localhost:5678` in a browser. The first visit shows the initialization page; set an administrator username and password (at least 6 characters) and sign in.

### 3. Obtain the enrollment key

On its **first start**, the Dashboard container prints the initial enrollment key to its logs:

```bash
docker logs uptimemesh-dashboard 2>&1 | grep -A1 "初始接入密钥"
```

You can also view and copy the current key from the Settings page after signing in (the plaintext is stored alongside the key). It can be rotated at any time (the old key stops working immediately; credentials of already-approved nodes are unaffected). The “copy install command” dialog on the Nodes page fills in this key automatically.

### 4. Deploy the first node (Agent)

**Option A (recommended): one-line install from the Nodes page.** Open the Nodes page → click **“copy install command”** at the top (the key is already filled in; adjust the node name if needed) → run the copied command as root on the target Linux host:

```bash
curl -fsSL http://<dashboard-host>:5678/api/v1/agent/install.sh | sudo sh -s -- \
  --server ws://<dashboard-host>:5678/ws/agent --key <enrollment-key> --name beijing-aliyun-01
```

The script picks the linux/amd64 or linux/arm64 package automatically via `uname -m` (served by the Dashboard), installs to `/usr/local/bin/uptimemesh-agent`, writes a systemd unit (start on boot, restart on crash, `CAP_NET_RAW` for ICMP PING), stores the key in `/etc/uptimemesh-agent/env` (readable by root only) and persists credentials under `/var/lib/uptimemesh-agent`. Afterwards, click **Approve** on the Nodes page.

**Option B: run the binary manually.**

```bash
# Build inside the repo: ./deploy/build-agent.sh (cross-compiles linux/amd64 and linux/arm64 into bin/)
./bin/uptimemesh-agent-linux-amd64 \
  --server ws://<dashboard-host>:5678/ws/agent \
  --key <enrollment-key> \
  --name beijing-aliyun-01 \
  --cred-dir /var/lib/uptimemesh-agent
```

**Option C: run the Agent in Docker.**

```bash
docker build -f deploy/Dockerfile.agent -t uptimemesh-agent .
docker run -d --name uptimemesh-agent --restart unless-stopped \
  --network host --cap-add NET_RAW \
  -v uptimemesh-agent-cred:/data \
  uptimemesh-agent \
  --server ws://<dashboard-host>:5678/ws/agent \
  --key <enrollment-key> --name idc-b --cred-dir /data
```

A node's first connection lands in **pending approval**. Click **Approve** on the Nodes page to issue its dedicated credential (persisted to `--cred-dir`, so later reconnects skip approval). To retire a node, first **revoke** it on the Nodes page (its credential stops working immediately) and then **delete** it; deletion is a soft delete — it disappears from the list while historical results still carry its node name, and if that machine connects again it re-enters pending approval as a new node.

> One-line install (Option A) relies on Agent binaries bundled with the Dashboard: the Docker image cross-compiles them during its build. When running the Dashboard outside a container, run `./deploy/build-agent.sh` first to produce `bin/uptimemesh-agent-linux-*`; the Dashboard distributes them from `bin/` automatically.

### 5. Create a monitor and wait for alerts

Monitors → **New monitor**:

| Setting | Description |
|---|---|
| Type | HTTP(S): method / headers / body / expected status code / keywords / JSON query assertion; PING: a single ICMP packet (one per node per round); TCP port: a successful connection is a success; Push: an external system reports to a dedicated URL; Download speed: each node downloads a file and measures throughput |
| Period | 10–3600 s (round frequency, default 60); download-speed monitors probe nodes serially within a round, so the effective interval is never shorter than the period |
| Timeout | Probe timeout, must be ≤ period |
| Threshold + consecutive breach rounds | Default `100% × 3 rounds`: only when this round's success rate stays below 100% for 3 consecutive rounds does the monitor flip to DOWN (for download-speed monitors the threshold is a speed floor, in `KB/s` or `MB/s`) |
| Assigned nodes | At least one **approved** node; “all nodes (including future ones)” and “exclude nodes” are also available, so newly added nodes need no edits |
| Notification channels | Create a global webhook on the Channels page first, then select it in the monitor |

Saving triggers an immediate probe round (no need to wait a full period), and the same happens when a paused monitor is resumed. The **Test** button runs **unsaved** configuration on one online node and shows the actual request and response details, without creating a round or storing results.

### 6. Configure notification channels

Create a webhook (`http(s)` URL) on the Channels page:

- Leave the request body template empty ⇒ a generic JSON payload is sent: structured fields (`event`, `monitorName`, `url`, `roundSuccessRate`, `errorCount`, `timestamp`, …) plus `title` / `content` rendered from your templates.
- Fill in a **request body template** ⇒ the payload matches the JSON structure expected by DingTalk / WeCom / Slack bots, with `{{title}}`, `{{content}}` and the event variables; values are escaped as JSON and the rendered result must be valid JSON (the dialog documents the variables and previews the result live).
- Wording is configured per event type (DOWN / UP / TEST) under Settings → Notification templates, with placeholders such as `{{monitorName}}`, `{{url}}`, `{{successRate}}`, `{{speed}}`, `{{agents}}`, `{{errorCount}}` and `{{timestamp}}`. `{{agents}}` is the **per-node detail** of the round that triggered the flip (one line per node) and `{{errorCount}}` is how many enabled monitors are still alerting. Clearing both fields restores the built-in defaults.
- The Channels page also supports “test send” and “apply to all monitors” (it appends the channel to every monitor without touching other channels).
- Every channel has an **enabled switch** (in the list row and in the dialog). A disabled channel receives no alert or recovery notices, while the monitor selections are kept — disabling is meant for temporarily muting a channel (for example while the group is busy with something else), and re-enabling resumes delivery immediately without touching any monitor. “Send test” ignores the switch, so a URL and template can be verified before enabling.

## Local development

```bash
# Dashboard (no external database required; creates dashboard/data/uptimemesh.db automatically)
cd dashboard && go run ./cmd        # port from dashboard/manifest/config/config.yaml (default 8000)

# Frontend (the Vite dev server proxies /api and /ws to 127.0.0.1:5678)
cd web && npm install && npm run dev   # http://localhost:5173
```

> Test code (`*_test.go` and the test helper packages) is not tracked in this repository and lives only in local development environments, so `go test ./...` in the public repo runs no cases.

> Containers and local runs can coexist: compose sets `SERVER_PORT=8000` so the container listens on 8000 and publishes host port 5678, while a local `go run` uses `config.yaml`. If you want a local Dashboard to match the frontend's default proxy, run `SERVER_PORT=5678 go run ./cmd` or change the proxy port in `web/vite.config.ts` to 8000. The SQLite file path comes from `SQLITE_PATH` (or `sqlite.path` in `config.yaml`).

> **No region shown for a local run?** The agent region is resolved from a local MaxMind database. The container image bundles one (downloaded at build time); a plain `go run` does not. From the **repository root**, run `sh deploy/fetch-geoip.sh` and start with the path set — without it, regions can only be set by hand and the page shows “Unknown”.
>
> ```bash
> cd dashboard && GEOIP_DB_PATH=../deploy/geoip/GeoLite2-Country.mmdb go run ./cmd
> ```
>
> The startup log prints `已加载地域库: …（数据版本 YYYY-MM-DD）` (“geolocation database loaded”, with its data version). The database is refreshed weekly — re-run the script when the data version gets stale. The data is GeoLite2, created by MaxMind; see the comments in the script for the public mirror used.

Run the static check before changing frontend internationalization:

```bash
cd web && npm run i18n:check   # scans for hard-coded Chinese; npm run build also validates zh/en key parity via vue-tsc
```

## Building and releasing

- **Docker (recommended)**: `cd deploy && docker compose up -d --build`. `Dockerfile.dashboard` runs `npm run build` inside the image and copies the output into the `go:embed` directory, so every `--build` ships the latest frontend with no manual copying.
- **Local, non-containerized**: `cd web && npm run build`, copy `web/dist/` into `dashboard/internal/webdist/dist/`, then `cd dashboard && go build -o uptimemesh ./cmd`. Note that `go build` only embeds a `webdist/dist/` that already exists, so always re-copy after frontend changes or you will keep serving the old pages.
- **Agent version number**: `AGENT_VERSION` in `deploy/docker-compose.yml` is compiled into the Agent binary (`-ldflags -X …agentclient.Version`) and written to the `VERSION` file in the distribution directory; the Nodes page uses it to decide “upgradable / needs reinstall”, so the two must match — `sh deploy/bump-agent-version.sh` updates both.

## Repository layout

```text
dashboard/        GoFrame server (REST + /ws/agent + /ws/browser + round scheduler + alerting)
  internal/
    store/        SQLite data access (pure-Go driver + schema.sql)
    hub/          Agent connection registry and WS sessions
    scheduler/    round creation / dispatch / finalization / missing-sample decisions
    alert/        alert state machine (pure functions)
    notifier/     webhook delivery (retries at 5/15/30 s)
    webhub/       real-time push to browsers
    retention/    expired-result cleanup
    api/ auth/ webdist/
agent/            node process (connect, probe, report, credential persistence, self-upgrade)
shared/           protocol and probe implementations (shared by agent and dashboard)
web/              Vue 3 + Vite + TS + Element Plus + ECharts frontend
deploy/           docker-compose, Dockerfiles, build-agent.sh, bump-agent-version.sh, .env.example
docs/             adr/ (decision records), protocol.md (WS frame contract), logo/ (brand assets), agents/ (collaboration guides)
```

## Documentation and contributing

- Domain glossary and conventions: [`CONTEXT.md`](CONTEXT.md) (Chinese)
- Architecture decision records: [`docs/adr/`](docs/adr/) (Chinese)
- Agent ↔ Dashboard frame protocol: [`docs/protocol.md`](docs/protocol.md) (Chinese)
- Collaboration conventions (issues, labels, layout, commit rules): [`docs/agents/`](docs/agents/) (Chinese)
- Every new user-facing string in the frontend must be internationalized (`web/src/i18n/`; Chinese and English entries must be changed together).
- Any change affecting the Agent binary (Go code under `agent/` or `shared/`, or how it is built) requires bumping the agent revision.

## Brand assets

Every logo file is generated from one shared set of geometry and colour values, so the SVG and the PNG can never drift apart — change a number and re-run:

```bash
python docs/logo/generate_logo.py            # regenerate everything
python docs/logo/generate_logo.py --check    # design self-check (contrast, small-size legibility, lockup balance, SVG validity)
```

| Use | Files |
|---|---|
| README / docs lockup (light + dark) | `docs/logo/uptimemesh-logo-horizontal{,-inverse}.svg` |
| Bare mark (works on light and dark) | `docs/logo/uptimemesh-mark.svg`, `uptimemesh-mark-on-dark.svg` |
| Avatar / app icon (rounded plate) | `docs/logo/uptimemesh-mark-badge.svg`, `uptimemesh-icon-{512,256,128,64,48,32,24,16}.png` |
| Favicon | `docs/logo/uptimemesh-favicon.ico` (16/24/32/48 frames) |
| Social card | `docs/logo/uptimemesh-og-banner.png` (1584×704) |
| Delivery sheet (review) | `docs/logo/uptimemesh-brand-sheet.png` |

- **Meaning**: the hexagon is the mesh of nodes spread across networks, the centre is the Dashboard, the ECG trace running through it is active probing (Uptime), and the amber node at the lower right is one that is alerting.
- **Palette shared with the UI**: brand blue `#2D7FF9` distilled from the Element Plus primary `#409eff`; online green `#3DDC84` and alert amber `#F5A623` mirror the up / breach status colours.
- **Small sizes use their own geometry**: full detail ≥128 px, simplified 33–127 px (thicker strokes, no hairline diagonals), minimal ≤32 px — scaling the large mark down makes the white hub dot and thin lines disappear under antialiasing.
- Rationale and usage rules: [`docs/logo/README.md`](docs/logo/README.md) (Chinese).

---

[中文](README.md) | **English**
