# Dashboard 不执行任何检测,检测只由 Agent 完成

与 Uptime Kuma(服务器本体也能执行监控)的本质区别:Dashboard 只做配置管理、任务下发、结果汇总与展示。即使 Dashboard 自身可达某目标,该目标的可用性与延迟也只以 Agent 上报的 Check Result 为准,保证统计数据口径单一、可信。

监控弹窗上的「测试」按钮同样受此约束:它由 Dashboard 挑一个在线节点、下发 `probe_test`
帧、等结果,再原样带回页面(见 `docs/protocol.md`)。"就测一次、跑在 Dashboard 上更快"
不是例外 —— 那会让页面上的结论与真实探测(不同出口 IP、不同地区)对不上。
