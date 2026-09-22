# Agent 与 Dashboard 之间使用 WebSocket 长连接通信

Agent 部署在各地网络(NAT/防火墙之后),Dashboard 无法反向连接 Agent,因此连接统一由 Agent 侧发起,建立后 Dashboard 通过该连接实时 push 任务与配置变更,Agent 回传检测结果与心跳,断线自动重连。曾考虑 HTTP 轮询(实现最简但下发延迟高)和引入消息队列(对当前规模过度设计),均被否决。
