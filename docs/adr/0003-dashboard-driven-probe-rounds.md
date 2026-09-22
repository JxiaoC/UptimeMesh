# 检测调度由 Dashboard 按轮次驱动,Agent 不自主按周期执行

需要把"同一次探测"的多个 Agent 结果放在一起聚合出该轮的成功率,因此每次探测必须由 Dashboard 统一发起(创建轮次、push 给被指派的 Agent、结果携带轮次 ID 回传)。曾考虑下发配置由 Agent 自治调度(断线不丢检测),但那样 Dashboard 无法判定上报结果属于哪一次探测,轮次间误差还会随时间漂移,故否决。

## Consequences

- Agent 离线期间的轮次没有它的结果,该轮缺样(用户已接受;缺失不计入可用率分母)。
- 轮次由 Dashboard 的定时器生成,单点;Dashboard 重启会中断调度。
