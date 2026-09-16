import zhOverview from '../zh-CN/overview'

const overview: typeof zhOverview = {
  overview: {
    totalCount: '{count} total',
    statUp: 'Healthy',
    statDown: 'Alerting',
    statUnknown: 'Unknown',
    statPaused: 'Paused',
    pausedHint: 'Paused monitors produce no new rounds and are not listed below (they still count in the total); view or resume them on the Monitors page',
    emptyWithPaused: 'No monitors are running ({count} paused, hidden from the overview)',
    empty: 'No monitors yet — create one on the Monitors page',
    groupMonitorCount: '{count} monitors',
    recentChangesTip: 'Latest {count} state flips; red = down, green = recovered (recovery rows also show how long the alert lasted). Click a row to open the monitor',
    recentChangesEmpty: 'No state changes yet (recorded only on flips)',
    columnChangedAt: 'Changed at',
    columnMonitor: 'Monitor',
    columnMetric: 'Value',
    changeDown: 'Down',
    changeUp: 'Recovered',
    changeFromTo: '{from} → {to}',
    everyPeriod: 'every {period}s',
    availability24h: '24h availability',
    availability7d: '7d availability',
    availability30d: '30d availability',
    lastSuccessRate: 'Latest success rate',
    lastSpeed: 'Latest speed',
    lastLatency: 'Latest latency',
  },
}

export default overview
