// 通用词条:应用外壳、公共动作、导航、登录页、监控类型、状态标签与错误兜底。
//
// 写词条的注意事项(踩过的坑都在这里):
//   1. vue-i18n 把 `{name}` 当插值、`|` 当复数分隔符、`@` 当链接消息。词条里需要
//      这些字符时不要直接写:改用参数传入(如 JSON 示例里的花括号)。
//   2. 不要在导出对象上加 `as const`:值会退化成字面量类型,英文词条就被要求逐字
//      相同了。`en-US/*.ts` 以 `typeof 本文件` 做类型标注,靠结构一致性保证 key 对齐。
export default {
  app: {
    title: 'UptimeMesh 监控中心',
    brand: 'UptimeMesh',
    subtitle: '分布式主动监控',
  },
  common: {
    confirm: '确定',
    cancel: '取消',
    save: '保存',
    close: '关闭',
    refresh: '刷新',
    add: '添加',
    edit: '编辑',
    delete: '删除',
    copy: '复制',
    deleted: '已删除',
    saved: '已保存',
    success: '操作成功',
    pause: '暂停',
    resume: '恢复',
    online: '在线',
    offline: '离线',
    unknown: '未知',
    ungrouped: '未分组',
    failed: '失败',
    none: '—',
    live: '实时',
    polling: '轮询',
    logout: '退出登录',
    neverReported: '从未上报',
    // 列表连接符:拼接节点名等多值文案时用,英文用逗号而非中文顿号。
    listSeparator: '、',
    clipboardDenied: '浏览器拒绝了剪贴板访问,请手动选中复制',
    copyFailedManual: '复制失败,请手动选中地址复制',
    pushUrlCopied: '上报地址已复制',
    // 上报地址可带的查询参数示例(msg/ping 的说明是展示文案,故按语言给;
    // status=up|down 纯代码且无中文,留在模板里即可)
    pushParamMsg: 'msg=说明',
    pushParamPing: 'ping=耗时毫秒',
    // 报警持续时长(状态变动记录里「恢复」那一行旁多出来的小卡片)。
    // 秒数到量级的折算在 utils/duration.ts 里做,词条只负责措辞。
    alertDuration: '报警持续时长',
    alertDurationTip: '报警持续时长:从判定报警(连续低于阈值达到配置轮数)到这次恢复之间的时长',
    durationTag: '持续 {duration}',
    durationSeconds: '{seconds} 秒',
    durationMinutesSeconds: '{minutes} 分 {seconds} 秒',
    durationHoursMinutes: '{hours} 小时 {minutes} 分',
    durationDaysHours: '{days} 天 {hours} 小时',
  },
  nav: {
    overview: '总览',
    monitors: '监控',
    agents: '节点',
    channels: '通知渠道',
    settings: '设置',
  },
  login: {
    titleInit: '初始化',
    titleLogin: '登录',
    initHint: '首次使用,请设置管理员账号密码',
    username: '用户名',
    password: '密码(≥6位)',
    submitInit: '创建并进入',
    submitLogin: '登 录',
    invalid: '请填写用户名与至少 6 位密码',
  },
  type: {
    http: 'HTTP(S)',
    ping: 'PING',
    tcp: 'TCP 端口',
    push: '外部上报',
    download: '下载速度',
  },
  status: {
    // displayState 的标签文案(UP/DOWN/UNKNOWN 是协议取值,两种语言都原样显示)
    UP: 'UP',
    DOWN: 'DOWN',
    UNKNOWN: 'UNKNOWN',
    PAUSED: '已暂停',
    waitingPush: '等待上报',
    // 轮次状态条的色块口径(术语与列表页图例一致:「破线」是内部行话,已统一为「低于阈值」;
    // down 是告警态已判 DOWN 的破线轮,面向用户只说「不达标」——与 monitors.legendDown 同一措辞)
    up: '达标',
    breach: '低于阈值',
    down: '不达标',
    noSample: '无样本',
    noRounds: '暂无轮次',
    roundTip: '{time} · {label} · 成功率 {rate}%',
    // 下载速度监控的状态条悬停:判定值是速度而不是成功率。
    roundTipSpeed: '{time} · {label} · 速度 {speed}',
  },
  errors: {
    requestFailed: '请求失败',
    network: '网络错误',
  },
}
