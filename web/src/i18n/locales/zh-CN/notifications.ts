// 通知记录页词条:历史通知投递的留痕列表(成功/失败都记,永久保留)。
export default {
  notifications: {
    title: '通知记录',
    // 长说明是本页最重要的语义交代:为什么这里的历史比检测结果长。
    retentionHint:
      '所有通知渠道的投递留痕(告警、恢复与测试消息,成功与失败都记录),数据永久保存,不受「原始结果保留(天)」的限制。',
    empty: '暂无通知记录',
    columnTime: '发送时间',
    columnEvent: '事件',
    columnMonitor: '监控',
    columnChannel: '通知渠道',
    columnTitle: '标题',
    columnStatus: '发送状态',
    columnActions: '操作',
    viewDetail: '详情',
    detailTitle: '通知详情',
    detailContentTitle: '标题',
    detailError: '失败原因',
    status: {
      success: '发送成功',
      failed: '发送失败',
    },
    event: {
      DOWN: '告警触发',
      UP: '恢复通知',
      TEST: '测试消息',
      AGENT_DOWN: '节点离线',
      AGENT_UP: '节点恢复',
    },
    // 尝试次数(重试过的才提示):「重试 N 次」指首次之外又试了 N 次。
    attemptsN: '共尝试 {n} 次',
    filterMonitor: '按监控筛选',
    filterChannel: '按渠道筛选',
    filterEvent: '按事件筛选',
    filterStatus: '按状态筛选',
    clearFilters: '清除筛选',
  },
}
