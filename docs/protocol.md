# Agent ↔ Dashboard WebSocket 帧协议

单一事实源为 `shared/protocol` 包,本文为契约说明。连接由 Agent 发起(ADR-0001),端点 `GET /ws/agent`(Upgrade)。线格式为 JSON 文本帧,统一外壳:

```json
{ "type": "<帧类型>", "payload": { } }
```

## 握手

1. **Agent → `hello`**:`{enrollment_key?, credential?, agent_name, agent_version, os, arch, capabilities?, ipv4_available?, ipv6_available?}`。凭据二选一:首次接入用全局接入密钥;批准后重连用专属凭据。`capabilities` 声明本进程支持的可选能力(当前有 `self_upgrade` 与 `probe_test`),老版本 Agent 不带该字段。`ipv4_available` / `ipv6_available` 是节点自报的**本机网络族可用性**(是否存在非回环、非链路本地的该族地址,见 `agentclient.DetectIPFamilies`):仪表盘用指针区分「上报了 false」与「老版本根本没上报」,后者在节点页显示为「未知」而不是「不支持」。
2. **Dashboard → `hello_ack`**:`{status: "pending"|"approved", agent_id, reason?}`。
   - `pending`:等待管理员批准,连接保持,Agent 应继续发心跳;
   - 鉴权失败 → Dashboard 发 `error`(code=`auth_failed`)后关闭连接。
3. 批准后(票 03)**Dashboard → `credential`**:`{agent_id, credential}`。Agent 必须持久化到 `--cred-dir`。

## 稳态帧

| 帧 | 方向 | 载荷 | 语义 |
|---|---|---|---|
| `heartbeat` | A→D | `{unix, ipv4_available?, ipv6_available?}` | 每 10s;丢失 3 次判离线。网络族可用性随心跳一起上报(网卡/路由变化不必等重连),Dashboard 只在取值变化时落库 |
| `ping` / `pong` | D→A→D | `{ping_id}` | Dashboard 探活。两处用途:①轮次定稿的缺样决策(票 06);②**周期保活** —— 每条已注册连接每 20s 收到一帧 `ping`,Agent 立即原样回 `pong`(见下文「链路保活」) |
| `probe_task` | D→A | `{round_id, monitor_id, monitor_name, monitor_type, check, deadline_unix}` | 一次轮次下发;`check` 为 checkconfig.HTTP / Ping / TCP / Download JSON(push 监控不下发任务,结果由外部上报产生) |

`checkconfig.HTTP` 的判定项按顺序生效:期望状态码 → 期望关键字(包含/禁止)→ **JSON 断言**(可选,`json_path` + `json_path_operator` + `expected_value`):用 JSONata 表达式从响应取值后与期望值比较,表达式为空表示不做该断言。三者都作用于**跟随重定向后的最终响应**(上限 10 跳,与 UptimeKuma 的 axios 行为一致)。语义见 `.scratch/json-assert/spec.md`。`checkconfig.TCP` 只做 TCP 连接(建立成功即成功),TCP 端口与外部上报见 `.scratch/tcp-push-monitors/spec.md`。

`ip_version`(HTTP / Ping / TCP / Download 四份 `check` 共有,取值 `auto` / `ipv4` / `ipv6`,空按 `auto`)约束**节点到目标**这一段探测走哪个协议族:节点把它折算成 `tcp4`/`tcp6`、`udp4`/`udp6`(HTTP/下载是拨号网络名,TCP 是拨号网络名,PING 是解析族 + ICMP/ICMPv6),`auto` 交回系统默认。它**不**影响节点↔Dashboard 的长连接;节点是否具备该族出口地址由节点页的「网络能力」标签回答(见握手)。

`checkconfig.Download`(下载速度监控)是**请求侧与 HTTP 相同、判定侧不同**的一份配置:节点把 URL 指向的文件完整下载一遍(只统计字节数与耗时,不落盘、不保留正文),2xx 表示下载成功;速度与是否达标都不在节点判定 —— 节点只回传测得的速度,Dashboard 按本次平均速度与监控的下载速度阈值比较(见 `.scratch/download-speed-monitor/spec.md`)。

下载速度监控的 `probe_task` 是**逐节点串行**下发的:一轮里同一时刻只有一个节点收到任务,上一份结果回来(或等满"探测超时 + 3s")之后才发下一个,`deadline_unix` 因此按节点数顺延(`N × (超时 + 3s) + 宽限期 + 探活预算`);上一轮未定稿不会开下一轮。其余类型仍是一轮全发。
| `probe_result` | A→D | `{round_id, monitor_id, ok, latency_ms, http_status?, error?, speed_kbps?, bytes?, started_at_unix, finished_at_unix}` | 结果回传;晚到结果由 Dashboard 标记 late 入库。`speed_kbps`(平均速度,KB/s)与 `bytes`(下载字节数)仅下载速度监控有值 |
| `probe_test` | D→A | `{test_id, monitor_type, check}` | 一次「测试」:跑一遍弹窗里**未保存**的配置,不建轮次、不落库 |
| `probe_test_result` | A→D | `{test_id, ok, latency_ms, http_status?, error?, speed_kbps?, bytes?, detail?}` | 测试结果 + 请求/响应明细(见下文「测试」一节) |
| `upgrade` | D→A | `{target_version, sha256}` | 一键升级:要求节点升到 `target_version`;`sha256` 是安装包内容校验和,校验不过节点拒绝替换 |
| `upgrade_result` | A→D | `{ok, version, error?}` | 升级结果回传(成败都回);成功后节点随即重启,新版本以一次新的 `hello` 重新接入 |
| `error` | D→A | `{code, message}` | 随后关闭连接;code: `auth_failed` / `protocol_error` / `credential_revoked` |

## 时序约束

- Agent 连接后 10s 内必须发送 `hello`,否则服务端断开。
- Agent 探测超时(= `check.timeout_seconds`)后立即回传失败结果,不得沉默。
- 同一 agent_id 重复连接:服务端替换旧连接。

## 链路保活(半开连接)

心跳是 A→D 的单向证据,**只够 Dashboard 判断 Agent 是否还在**;反过来 Agent 判断不了
链路是否还活着:TCP 半开(对端已丢弃、本端不知道)时本地写会被中转(frp)侧 ACK 掉、
读又永久阻塞,Agent 会一直"以为自己在线",而 Dashboard 早已判离线并掐断连接 ——
两侧状态长期背离(节点日志停在"已接入",节点页却显示离线),只能人工重启 Agent 恢复。

因此约定一对参数,改动时必须成对:

| 侧 | 参数 | 值 | 语义 |
|---|---|---|---|
| Dashboard | `hub.KeepaliveEvery` | 20s | 向每条已注册连接(含 pending)下发一帧 `ping`,不等 `pong` |
| Agent | `agentclient.readIdle` | 60s | 连续这么久**一帧都没收到**(含保活 `ping`)即认定链路已死,断开并按指数退避重连 |

保活 `ping` 的 `pong` 回包没有等待方,由 Dashboard 的收帧循环直接丢弃;它不承担延时统计,
只负责让 Agent 的读侧有活可干。`readIdle` 必须显著大于 `KeepaliveEvery`(当前 3 倍)——
**旧 Dashboard(不发保活)+ 新 Agent 会让 Agent 每 60s 空转重连一次**,故两者必须同时发布
(改了 Agent 就要重建仪表盘镜像,见 AGENTS.md「Agent 版本号」)。

## 测试(probe_test)

监控新建/编辑弹窗上的「测试」按钮:把**还没保存**的探测配置交给一个在线节点当场跑一次,
页面据此显示判定与"发了什么请求、返回了什么"。它与轮次是两条路:

- **不建轮次、不落库**:导入结果表会污染可用率、状态条与告警判定(轮次只由调度器按周期
  创建,ADR-0003),所以结果只回给发起测试的那次 HTTP 请求;
- **判定口径完全相同**:Dashboard 用 `scheduler.CheckPayload` 组装 check(与建轮次同一个
  函数),节点用 `shared/probe.ExecuteTest` 执行(与轮次同一个 `runHTTP`),避免"测试通过、
  轮次却总失败";
- **Dashboard 仍然不探测**(ADR-0002):它只挑节点、下发帧、等结果,执行永远在节点上。

`probe_test_result.detail` 是排障用的明细(失败时页面自动展开):

| 字段 | 含义 |
|---|---|
| `method` / `url` / `headers` / `body` | 实际发出的请求(HTTP;`headers` 是配置里的请求头) |
| `target` | 真正建连的目标:HTTP 按 Host 头选源时是那个 IP(`URL` 仍是虚拟主机名);ping/tcp 是"主机"或"主机:端口" |
| `status` / `status_text` / `resp_headers` | 响应状态行与响应头 |
| `body_excerpt` / `body_bytes` / `body_truncated` | 响应体前 8KB;总字节数与是否截断如实回报 |
| `body_note` | 没有正文的原因(二进制内容/无 Content-Type 等) |
| `final_url` / `redirects` | 跟随重定向后的最终地址与跳转链(上限 10 跳) |

约束与前提:

- 节点必须在 `hello` 里声明 `probe_test` 能力(0.1.4 起才有)。老节点不认识该帧、收到只会
  静默忽略,因此 Dashboard 直接拒绝并提示「请在节点页一键升级」,而不是让页面干等超时。
- 只能测 `http` / `ping` / `tcp` / `download`:外部上报没有可探测的目标,页面不给按钮。
  下载速度监控的测试会真下载一遍,结果里额外带 `speed_kbps` 与 `bytes`(弹窗展示"有多快")。
- 超时:节点侧就是 `check.timeout_seconds`,Dashboard 再多等 5s;没等到就回一句"节点没有
  返回测试结果"(仍然回 `code=0` 的结果体,页面把它显示在测试面板里)。
- 反转模式:页面显示的 `ok` 是**有效判定**(已按反转口径取反),`probeOk` 是节点的原始判定,
  两者都被带回,页面写明"原始探测成功/失败"。

## 一键升级(自升级)
节点页对「版本与仪表盘分发包不一致」、且**声明了 `self_upgrade` 能力**的已批准节点提供一键升级
(能力协商见握手:老版本 Agent 不认识 `upgrade` 帧,收到只会静默忽略,因此不能对它下发;
页面这类节点标「需重装」,要重新执行一次安装命令)。链路全部复用既有设施:

1. 页面 `POST /api/v1/agents/{id}/upgrade`;Dashboard 读取分发目录里的安装包与 `VERSION`
   标记,算出 sha256,下发 `upgrade` 帧(仅目标版本 + 校验和,不代传二进制)。
2. 节点按自身 `--server` 接入地址推导下载地址(`ws→http` / `wss→https`,取 host,
   与 `install.sh` 同规则),从 `/api/v1/agent/download/linux-<arch>` 拉取安装包。
3. 节点校验 sha256,通过后把新二进制原子替换到自身路径(`rename`,不触发 `ETXTBSY`),
   回 `upgrade_result`,再 `exec` 自身换上新的进程映像(PID 不变,systemd 不受影响)。
4. 新版本以凭据重连,Dashboard 在 `hello` 时刷新库内版本与能力,节点页的「可升级」消失。

约束与前提:

- 仅 Linux 节点可自升级(分发目录只有 Linux 二进制);Windows 上运行中的可执行文件
  被系统占用,无法就地替换,节点也不声明该能力、页面不给升级按钮。
- 节点必须对自身可执行文件所在目录有写权限(源码安装/systemd 部署默认 root 即可;
  容器部署见 `deploy/Dockerfile.agent` 对二进制的 `chown`)。
- `sha256` 缺失一律拒绝执行;替换后若 `exec` 失败,新版本已就位,重启服务即生效。
- 分发目录内的 `VERSION` 必须与二进制内 `-ldflags -X ...agentclient.Version` 打标的
  版本一致(见 `deploy/build-agent.sh`、`deploy/Dockerfile.dashboard`),否则会反复提示可升级。
- 版本号相同**不代表二进制相同**:镜像构建用的 `AGENT_VERSION` 是固定值时,改了 Agent 侧
  探测行为而没 bump 版本,老节点会一直显示「已是最新」。因此 Dashboard 额外把「Linux 节点
  一个能力都不声明」也算作需要升级(它一定比能力协商还旧),页面标「需重装」;真正的
  解法是发布新 Agent 时 bump `AGENT_VERSION`。

## 浏览器实时推送(票 10)

端点 `GET /ws/browser`(Upgrade,只读推送,无需 JWT)。服务端首帧推送 `hello`。之后按 JSON 文本帧推送:

服务端每 30s 发一次 WS 控制帧 `ping`(浏览器自动回 `pong`)维持长连接;读截止(90s)靠
`pong` 续期。**浏览器只发一种上行帧**:监控列表页要一次「最近状态」快照
(`{"type":"monitor_strips","rounds":50}`,见下表)—— 少了 ping,每条连接都会在 90s 后被
服务端自己的读截止掐断(前端只是静默重连,但断连窗口里的事件就丢了)。

```json
{ "type": "<事件>", "data": { } }
```

| 事件 | 触发时机 | data 载荷 |
|---|---|---|
| `hello` | 连接建立 | 无 |
| `round_finalized` | 每次轮次定稿(含 UNKNOWN) | `{monitorId, roundId, state, successRate, speedKbps?, scheduledAt, roundStatus, displayState, alertState, consecutiveBreaches, latencyMs, agents:[{agentId, ok, latencyMs, error}]}` |
| `monitor_flipped` | 监控告警状态翻转(DOWN/UP) | `{monitorId, name, alertState, roundSuccessRate, displayState, consecutiveBreaches, roundId, fromState, changedAt, type, speedUnit, speedKbps}`(`toState` 就是 `alertState`) |
| `monitor_changed` | 监控新建/编辑/暂停/恢复 | 与 `GET /api/v1/monitors` 的一行同构(`displayState`/`alertState`/`consecutiveBreaches`),**另带 `recentRounds`**(整表快照已不含色块,单行推送仍然带,免得为了这一行再去要一次快照) |
| `monitor_deleted` | 监控被删除 | `{monitorId}` |
| `monitors_changed` | 批量暂停/恢复(`POST /api/v1/monitors/batch`)、把渠道设为所有监控的渠道(`POST /api/v1/channels/{id}/apply-all`) | `{monitors: [<行>]}`;**不含 `recentRounds`**(启停/改勾选不改变历史色块,前端就地更新时保留原有的) |
| `monitors_deleted` | 批量删除 | `{monitorIds: [...]}` |
| `agent_changed` | 节点连接建立/断开、审批状态变化 | `{agentId, name, online}` |
| `monitor_strips` | **浏览器请求**「最近状态」快照时(`{"type":"monitor_strips","rounds":N}` 上行帧) | `{rounds, total, seq, strips:[{monitorId, cells:[{status, scheduledAt, successRate, speedKbps?}]}]}`,分块发出(`seq` 从 0 递增,每帧最多 25 个监控) |

**「最近状态」为什么单独走一条道**:那一列是每个监控 N 格历史色块(格数取后台设置
「最近状态格数」,默认 50、上限 200,见 `.scratch/status-strip-rounds-setting/spec.md`),
线上 200+ 监控时就是上万格、约 1MB —— 而列表页**整张表要等它到齐才渲染**,进页面因此要
等好几秒。所以 `GET /api/v1/monitors` 只回配置 + `displayState`(响应体只随监控条数增长),
色块改由浏览器在 WS 上单独要一次快照(服务端按 25 个监控一块分帧推送,首块几十毫秒就到,
页面上的色块一片片长出来),之后的新轮次仍由 `round_finalized` 增量追加。快照是**点对点**
应答(只发给发请求的那条连接),不是广播 —— 否则每个开着总览页的标签页都会白收上万格。
前端在挂载/回到本页/重连后各要一次(`connectionId` 变了必然重发);WS 不可用(被代理挡掉、
断线)时退回 `GET /api/v1/monitors/strips?rounds=N`(载荷同形,不切块,见
`.scratch/monitors-lazy-strip/spec.md`)。

**批量操作为什么要单独两个事件**:浏览器连接的发送队列只有 32 帧,列表页一次多选暂停/删除
可能涉及上百个监控 —— 逐行发 `monitor_changed` / `monitor_deleted` 会瞬间灌满队列,慢客户端
被成片丢帧。因此批量操作合成一帧(载荷是若干轻量行 / 一个 ID 数组);单行操作仍走
`monitor_changed` / `monitor_deleted`。两条路径都做了幂等处理:前端按 id 就地增删改,
重复或漏收都由兜底轮询修正。

**载荷为什么这么全**:监控列表页有 200+ 监控时,"每个轮次定稿就整表重拉一次 /monitors"(过去每行还带一条状态条)会把页面和 Dashboard 都压垮。因此 `round_finalized` 直接带上该监控最新的 `displayState`,以及这一轮的状态条单元(`scheduledAt`/`roundStatus`/`successRate`,下载速度监控另有 `speedKbps`),前端就地更新那一行即可(改行、追加色块、增删行,都不再发 HTTP)。色块口径(up=达标 / breach=低于阈值未判 DOWN / down=已判 DOWN / unknown=无样本)由 `webhub.RoundCellStatus` 统一给出 —— 它比较的是**与监控阈值同单位的判定值**(成功率监控是本次成功率,下载速度监控是本次平均速度,KB/s),与快照取数的 `api.roundStatusStrip` 同源 —— 后者的历史色块用告警状态机(`alert.Apply`)从窗口最老一轮重放得出,所以"红=故障、黄=预警"和真实判定永远一致(见 `.scratch/status-strip-down-color/spec.md`)。

**总览页的同一份载荷**:`latencyMs`(本轮按时回传样本的平均延时)与 `agents`(本轮各指派节点的 `ok`/耗时/失败原因,顺序 = 监控配置的指派列表)是给**总览卡片**用的 —— 卡片的「最新延时」与「节点明细」随每一轮变化,没有这两个字段,总览页就只能"一有事件就重拉整页"(改动前正是如此:每个 `round_finalized` 都发 4 个请求,20 个监控≈80 请求/分钟、200 个≈800 请求/分钟)。节点明细由 `api.agentTiles` 统一组装,与 `GET /overview` 的同名字段逐项一致(顺序、缺样文案"超时(节点在线)"/"节点离线"/"未派发任务"都只有一份实现 —— 第三种是任务从未交到节点手上的样本,见 `.scratch/restart-first-round/spec.md`)。

`monitor_flipped` 同时带上**这次翻转留下的那条状态变动记录**(`roundId`/`fromState`/`changedAt`/`durationSec`/`type`/`speedUnit`/`speedKbps`):总览页的「最近状态变动记录」是一张跨监控流水,收到后直接插一行,不必再拉 `/state-changes`。记录与翻转是 1:1 的同一件事(调度器先落记录再回调),字段与 `GET /api/v1/state-changes` 的那一行逐项一致 —— 包括 `changedAt`:变动记录存的是 UTC 秒,两边都经 `api.fmtTime` 转**服务端本地时区**渲染(容器默认 `TZ=Asia/Shanghai`,即 +8),所以推送与接口逐字相同(两处若用不同时区,同一个表格里"推送来的行"会比"快照拉回的行"差一个时区;所有时间展示字段都只走 `api.fmtTime` 这一个口径)。

`durationSec` 是**本次报警的持续秒数**,只有恢复(DOWN→UP)那条非 0:等于上一条报错记录到这次恢复的间隔(起点是状态机判 DOWN 的那一刻,不是"第一次低于阈值那一轮")。它由**调度器在落记录时算好写库**(回查本监控最近一条 `to_state=DOWN` 的记录),推送与两个 state-changes 接口读的是同一个数 —— 前端会同时拿到"推送插进来的行"与"快照拉回来的行",在读接口里现算迟早让同一行显示成两个时长。0 表示**没有可配对的报错记录**(监控开局就是 DOWN、或配对记录已被删除),前端按"无时长"处理,不显示「持续 0 秒」;存量库补列同样补成 0,不回填历史。见 `.scratch/alert-duration/spec.md`。

前端策略:推送驱动**增量**更新;WS 在线时 30s 兜底对齐一次,断线恢复 5s 并在重连后先整表对齐一次(监控列表页的重连对齐还包含重发一次状态条快照请求);服务端对慢客户端丢帧不背压,漏掉的帧由兜底轮询修正。**按推送更新的页面(总览/监控列表/监控详情/节点页)都必须实现这条兜底**:只订阅事件而不轮询的页面,一旦在断连窗口错过 `agent_changed`(节点上下线)就会**永久**显示错的在线状态,直到下一次事件或手动刷新。

总览页的 HTTP 只留两处:①进页面/点「刷新」的一次快照(`GET /overview` + `GET /state-changes?limit=20` + `GET /agents` + `GET /agents?includeDeleted=1`);②上面那条兜底(30s / 断线 5s)。其中**唯一不推送的字段是卡片的可用率窗口(24h/7d/30d)**:它来自小时聚合,是慢变量,由快照刷新即可(见 `.scratch/overview-websocket/spec.md` 的非目标)。另外,推送是**稠密**的(一次全量扫描上百条),前端把事件先入队、250ms 防抖 / 1s 上限合并后一次性落地,避免"每条事件一次整页渲染"。
