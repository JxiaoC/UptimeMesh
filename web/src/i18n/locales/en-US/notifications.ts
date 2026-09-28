// English counterpart of zh-CN/notifications.ts — key-for-key identical.
// The type annotation below is the compile-time gate: a missing or extra key
// fails `npm run build` (vue-tsc) instead of falling back to Chinese silently.
import type notifications from '../zh-CN/notifications'

const enNotifications: typeof notifications = {
  notifications: {
    title: 'Notification Logs',
    retentionHint:
      'Delivery records for every notification channel (alert, recovery and test messages, both successful and failed). Logs are kept permanently and are not subject to the "raw result retention (days)" setting.',
    empty: 'No notification logs yet',
    columnTime: 'Sent At',
    columnEvent: 'Event',
    columnMonitor: 'Monitor',
    columnChannel: 'Channel',
    columnTitle: 'Title',
    columnStatus: 'Delivery Status',
    columnActions: 'Actions',
    viewDetail: 'Details',
    detailTitle: 'Notification Details',
    detailContentTitle: 'Title',
    detailError: 'Failure Reason',
    status: {
      success: 'Delivered',
      failed: 'Failed',
    },
    event: {
      DOWN: 'Alert Triggered',
      UP: 'Recovery',
      TEST: 'Test Message',
      AGENT_DOWN: 'Agent Offline',
      AGENT_UP: 'Agent Back Online',
    },
    attemptsN: '{n} attempts in total',
    filterMonitor: 'Filter by monitor',
    filterChannel: 'Filter by channel',
    filterEvent: 'Filter by event',
    filterStatus: 'Filter by status',
    clearFilters: 'Clear filters',
  },
}

export default enNotifications
