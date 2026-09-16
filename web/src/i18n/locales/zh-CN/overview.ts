// 总览页词条。
export default {
  overview: {
    totalCount: '共 {count} 个',
    statUp: '正常',
    statDown: '报警',
    statUnknown: '未知',
    statPaused: '已暂停',
    pausedHint: '暂停的监控不产生新轮次,不在下方列表中显示(总数里仍计入);可在「监控」页查看与恢复',
    emptyWithPaused: '暂无在跑的监控({count} 个已暂停,不在总览中显示)',
    empty: '暂无监控,请先在「监控」页创建',
    groupMonitorCount: '{count} 个监控',
    // 最近状态变动记录:标题复用 monitorDetail.stateChangesTitle(同一个概念只有一份词条),
    // 这里只放总览特有的说明与列名。
    recentChangesTip: '最近 {count} 条状态翻转;红=报错、绿=恢复(恢复行并标出本次报警持续时长),点击一行进入监控详情',
    recentChangesEmpty: '暂无状态变动(仅状态翻转时记录)',
    columnChangedAt: '变动时间',
    columnMonitor: '监控',
    columnMetric: '判定值',
    // 变动结论只写「报错 / 恢复」,完整翻转(UP → DOWN)放悬停提示:在列里读箭头太费眼。
    changeDown: '报错',
    changeUp: '恢复',
    changeFromTo: '{from} → {to}',
    everyPeriod: '每 {period}s',
    availability24h: '24h 可用率',
    availability7d: '7d 可用率',
    availability30d: '30d 可用率',
    lastSuccessRate: '最新成功率',
    // 下载速度监控的卡片:第 4 格展示最近一轮的平均速度(单位随监控配置)。
    lastSpeed: '最新速度',
    lastLatency: '最新延时',
  },
}
