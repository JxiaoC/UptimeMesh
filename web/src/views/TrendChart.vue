<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import * as echarts from 'echarts'
import { flagClass, flagUrl } from '../utils/region'

interface Bucket {
  bucket: string
  bucketAt: number
  availability: number | null
  avgLatencyMs: number | null
  /** 下载速度监控:桶内平均速度(KB/s);其余类型为 null。 */
  avgSpeedKbps: number | null
  speedCount?: number
  valid: number
  success: number
  rounds: number
}
// AgentSeries 单个节点的序列(按 bucketAt 对齐到主桶轴)。
// 画延时还是速度由 metric 决定:下载速度监控传各节点的 KB/s,其余传各节点的延时(ms);
// 换算到展示单位(速度随 speedUnit)由本组件负责 —— 它才知道当前口径与单位。
interface AgentSeries {
  name: string
  region?: string
  data: (number | null)[]
}
// withDefaults:metric 决定整张图的口径 ——
//   rate :可用率(0~100,左轴)+ 平均延迟与各节点延迟(ms,右轴)
//   speed:平均下载速度(左轴)+ 各节点下载速度(左轴),单位随监控配置
//
// 两种口径的**曲线条数与纵轴数都不同**:速度口径没有延时曲线,右轴也随之隐藏
// (见 render 里的 secondSeries 与 yAxis 说明)。所以这里不写"第二条曲线永远是延迟",
// 而是按 metric 分派 —— 一个下载速度监控的图上不该出现 ms 刻度。
const props = withDefaults(defineProps<{
  data: Bucket[]
  /** 各节点序列:速度口径给 KB/s,可用率口径给 ms(见 AgentSeries)。 */
  agents?: AgentSeries[]
  metric?: 'rate' | 'speed'
  speedUnit?: string
  /**
   * 监控配置的告警阈值,**与主曲线同单位**(随 metric 而定):可用率是 %,
   * 下载速度用 speedUnit。画成一条虚线,让人一眼看出数据点落在阈值的哪一侧。
   * null / 缺省不画(push 监控没有可配置的阈值,由调用方传 null)。
   */
  threshold?: number | null
}>(), {
  agents: () => [],
  metric: 'rate',
  speedUnit: '',
  threshold: null,
})

const { t, locale } = useI18n()

// 速度曲线的展示单位换算:接口与阈值统一用 KB/s,图上的刻度按监控配置的单位。
const speedDivisor = computed(() => (props.speedUnit === 'MB/s' ? 1024 : 1))

const el = ref<HTMLDivElement>()
let chart: echarts.ECharts | null = null
// 容器尺寸观察者(见 onMounted 的说明):弹窗里的图表要跟着容器宽高变化重画。
let ro: ResizeObserver | null = null

// 节点曲线配色:与可用率/总延迟两支主色区分开。
const AGENT_COLORS = ['#f56c6c', '#409eff', '#67c23a', '#e6a23c', '#9b59b6', '#00bcd4', '#ff9f43', '#8e44ad']

// 系列名/轴名在 render() 里取 t():canvas 不会自己重渲染,语言切换由下面的 watch 触发重画。

// 与 ECharts 默认悬浮提示一致的排版参数。
const TOOLTIP_NAME_STYLE = 'font-size:12px;color:#6e7079;font-weight:400'
const TOOLTIP_VALUE_STYLE = 'font-size:14px;color:#464646;font-weight:900'

const agentOf = (name: string) => props.agents.find((a) => a.name === name)

// valueUnits 按系列下标给出悬浮提示里的数值单位(render 每次重建)。
// 提示里的节点名只有节点名本身、不带任何单位,裸数字(1218.19)看不出是毫秒还是别的口径,
// 所以值后面统一补单位。两种口径各自内部同单位:可用率监控是 %(主曲线)+ ms(平均延迟
// 与各节点延迟),下载速度监控**全是速度**(平均速度与各节点速度),单位即监控配置的单位。
let valueUnits: string[] = []

/** 悬浮提示走 HTML,可以直接用 flag-icons 的 CSS 类画旗。 */
function tooltipFormatter(params: unknown): string {
  const arr = (Array.isArray(params) ? params : [params]) as { seriesName: string; seriesIndex?: number; marker: string; value: unknown; axisValueLabel?: string }[]
  if (!arr.length) return ''
  const head = `<div style="font-size:12px;color:#6e7079;line-height:1">${esc(arr[0].axisValueLabel ?? '')}</div>`
  const rows = arr.map((p) => {
    const cls = flagClass(agentOf(p.seriesName)?.region || '')
    const flag = cls
      ? `<span class="${cls}" style="display:inline-block;width:15px;height:11px;border-radius:2px;flex:none"></span>`
      : ''
    const unit = p.seriesIndex == null ? '' : valueUnits[p.seriesIndex] ?? ''
    // 无数据时不补单位('—' + 'ms' 会读成有值)。
    const text = p.value == null || p.value === '' ? esc(readable(p.value)) : esc(readable(p.value)) + esc(unit)
    return '<div style="display:flex;align-items:center;gap:4px;margin-top:8px;line-height:1">'
      + p.marker
      + flag
      + `<span style="${TOOLTIP_NAME_STYLE}">${esc(p.seriesName)}</span>`
      + `<span style="margin-left:auto;padding-left:20px;${TOOLTIP_VALUE_STYLE}">${text}</span>`
      + '</div>'
  })
  return head + rows.join('')
}

function esc(v: unknown): string {
  return String(v ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c] as string
  ))
}

function readable(v: unknown): string {
  if (v == null || v === '') return '—'
  const n = Number(v)
  return Number.isFinite(n) ? String(Math.round(n * 100) / 100) : String(v)
}

// thresholdValue 阈值在主曲线口径下的数值;缺省 / 非有限值返回 null(不画)。
// 阈值与主曲线同单位(见 props.threshold),所以不做换算 —— 下载速度监控的阈值
// 存储单位就是 SpeedUnit,主曲线刻度也按同一单位换算(见 speedDivisor)。
function thresholdValue(): number | null {
  const v = props.threshold
  return v == null || !Number.isFinite(v) ? null : v
}

// thresholdMarkLine 把阈值画成一条虚线:让人一眼看出数据点落在阈值的哪一侧。
// silent:悬浮不弹提示 —— 它不是数据系列,跟着 axis 提示多出一行"阈值"只会干扰。
// 标注只写数值与单位:阈值本身与主曲线同单位,写单位是为了和左轴刻度对齐着读。
//
// 单位前的空格随口径而变,与仓库既有排版一致:百分比紧贴数字(「阈值 80%」,同
// 监控表单的「可用率阈值(%)」与表格的「<95% ×3轮」),速度单位前留一个空格
// (「阈值 2048 KB/s」,同 utils/speed.ts 的 formatSpeed 与表格的「<10 MB/s ×2轮」)。
function thresholdMarkLine(v: number): Record<string, unknown> {
  const text = props.metric === 'speed'
    ? `${readable(v)} ${props.speedUnit || 'KB/s'}`
    : `${readable(v)}%`
  return {
    silent: true,
    symbol: 'none',
    data: [{ yAxis: v }],
    lineStyle: { type: 'dashed', width: 2, color: '#f56c6c' },
    label: {
      // 放在网格内侧右上:贴着线、不越出绘图区(右侧只留了 48px,画在外面会被裁)。
      position: 'insideEndTop',
      formatter: `${t('chart.threshold')} ${text}`,
      color: '#f56c6c',
      fontSize: 12,
    },
  }
}

function render() {
  if (!chart) return
  const isSpeed = props.metric === 'speed'
  // 第一条曲线的名字与口径随监控类型而变:成功率监控画可用率,下载速度监控画速度。
  // 速度口径下主曲线是**各节点样本的加权平均**(后端 speed_sum/speed_count),
  // 所以名字写全「平均下载速度」—— 它与下面各节点自己的速度是同一个量纲,
  // 写「下载速度」会让人以为它属于某个节点。
  const PRIMARY_NAME = isSpeed ? t('chart.speed') : t('chart.availability')
  const LATENCY_NAME = t('chart.avgLatency')
  const data = props.data
  // 桶展示文本由后端按颗粒度格式化(天桶到日期、小时桶到小时);去年份前缀更紧凑。
  const buckets = data.map((d) => d.bucket.slice(5))
  const primary = data.map((d) =>
    isSpeed
      ? (d.avgSpeedKbps == null ? null : d.avgSpeedKbps / speedDivisor.value)
      : d.availability,
  )
  // 点太稀时折线无长度可画(单点甚至完全空白),退化为显示数据点符号
  const showSymbol = data.length < 8
  // 阈值:null 表示不画(见 props.threshold 的说明)。
  const th = thresholdValue()
  const markLine = th == null ? null : thresholdMarkLine(th)

  // 速度口径下没有第二条主曲线:延时对"这个文件下得快不快"没有解释力,而
  // 「平均速度」这件事已经由主曲线承担(它就是全体样本的平均),再画一条同数据的线
  // 只会完全重合。可用率口径保留延时曲线 —— 它在那边是独立的右轴口径。
  const secondSeries: echarts.SeriesOption[] = isSpeed ? [] : [{
    name: LATENCY_NAME, type: 'line', yAxisIndex: 1, smooth: true, showSymbol,
    data: data.map((d) => d.avgLatencyMs), itemStyle: { color: '#409eff' }, lineStyle: { width: 2 },
  }]

  const names = [PRIMARY_NAME, ...secondSeries.map((s) => s.name as string), ...props.agents.map((a) => a.name)]
  // 图例在 canvas 上绘制,国旗只能用富文本背景图(rich.backgroundColor.image)。
  const rich: Record<string, unknown> = {}
  const richKey = new Map<string, string>()
  props.agents.forEach((a, i) => {
    const url = flagUrl(a.region || '')
    if (!url) return
    const key = `fl${i}`
    rich[key] = { width: 15, height: 11, backgroundColor: { image: url }, padding: [0, 4, 0, 0] }
    richKey.set(a.name, key)
  })

  // 各节点曲线与第二条主曲线同口径同轴:速度口径画各节点自己的速度(共用速度轴),
  // 其余画各节点延时(共用延迟轴),便于横向比较。
  // agents 的数据由调用方按当前 metric 给对应口径:速度监控给 KB/s,其余给 ms
  // (见 props.agents 的说明)。速度的展示单位换算在这里做,只有组件知道 speedUnit。
  const agentYAxis = isSpeed ? 0 : 1

  const series: echarts.SeriesOption[] = [
    {
      name: PRIMARY_NAME, type: 'line', yAxisIndex: 0, smooth: true, showSymbol,
      data: primary, itemStyle: { color: '#67c23a' },
      areaStyle: { color: 'rgba(103,194,58,0.12)' },
      // 阈值虚线挂在主曲线上:它与主曲线共用左轴,标记才能落在正确的数值高度。
      ...(markLine ? { markLine } : {}),
    },
    ...secondSeries,
    ...props.agents.map((a, i) => ({
      name: a.name, type: 'line' as const, yAxisIndex: agentYAxis, smooth: true, showSymbol,
      data: isSpeed ? a.data.map((v) => (v == null ? null : v / speedDivisor.value)) : a.data,
      itemStyle: { color: AGENT_COLORS[i % AGENT_COLORS.length] },
      lineStyle: { width: 1, type: 'dashed' as const },
      connectNulls: false,
    })),
  ]

  // 悬浮提示的数值单位(与上面 series 的下标一一对应,见 valueUnits 的说明):
  // 可用率口径是 %(主曲线)+ ms(延时曲线与各节点);速度口径全是速度,同一个单位。
  const speedUnitText = props.speedUnit || 'KB/s'
  valueUnits = series.map((_, i) => {
    if (isSpeed) return speedUnitText
    return i === 0 ? '%' : 'ms'
  })

  chart.setOption({
    tooltip: { trigger: 'axis', formatter: tooltipFormatter },
    legend: {
      data: names,
      type: 'scroll',
      formatter: (name: string) => {
        const key = richKey.get(name)
        // 节点名里若含富文本语法字符({}|)就不套富文本,避免解析串味。
        return key && !/[{}|]/.test(name) ? `{${key}|}${name}` : name
      },
      textStyle: { rich },
    },
    // 右轴(延时)只给可用率口径用:速度口径下没有一条曲线挂在它上面,画出来就是
    // 一条光秃秃的「延迟(ms)」刻度,读者还得先确认"这条轴是空的"。轴数组长度保持
    // 两个不动,只把 show 关掉 —— series 的 yAxisIndex 语义(0=速度/可用率,1=延时)
    // 因此恒定,不必随口径改下标。
    grid: { left: 48, right: isSpeed ? 24 : 48, top: 70, bottom: 30 },
    xAxis: { type: 'category', data: buckets, boundaryGap: data.length < 2 },
    yAxis: [
      // 可用率固定 0~100;速度没有上限(按数据自适应),轴名带监控配置的单位。
      //
      // 速度轴的下界要**带上阈值**:markLine 不参与轴范围计算,若阈值低于所有实测速度,
      // 线会画到绘图区外面看不见 —— 而那恰恰是"全线达标"最该看到的情形。
      isSpeed
        ? {
          type: 'value',
          name: `${t('chart.axisSpeed')}(${props.speedUnit || 'KB/s'})`,
          splitLine: { show: false },
          // markLine 不参与轴范围计算,所以上下界都要把阈值算进去,否则线会画到绘图区外
          // (阈值高于所有实测速度=全线破线,低于所有实测速度=全线达标,两种情况都常见)。
          min: th == null ? undefined : ({ min }: { min: number }) => Math.min(min, th),
          max: th == null ? undefined : ({ max }: { max: number }) => Math.max(max, th),
        }
        : { type: 'value', name: t('chart.axisAvailability'), min: 0, max: 100, axisLabel: { formatter: '{value}%' } },
      { type: 'value', name: t('chart.axisLatency'), splitLine: { show: false }, show: !isSpeed },
    ],
    series,
    // yAxis 用 replaceMerge 整体替换而不是合并。
    //
    // 面板是**同一个图表实例**先按可用率画一次(monitor 还没加载完时 metric 取默认值
    // 'rate'),拿到监控配置后若是下载速度监控再按速度重画。setOption 默认会**合并**,
    // 于是第一遍写下的 `min: 0, max: 100` 与 `axisLabel.formatter: '{value}%'` 会留在
    // 速度轴上:轴名写着「速度(KB/s)」,刻度却是 0~100 的百分比,而且速度曲线被压到
    // 网格顶部 —— 阈值线的标注(2048 KB/s)与刻度(2,048%)对不上就是这么来的。
    // replaceMerge 让每次渲染的轴配置都从零生效,不再有上一次口径的残留。
  }, { replaceMerge: ['yAxis'] })
}

onMounted(() => {
  if (el.value) {
    chart = echarts.init(el.value)
    render()
    window.addEventListener('resize', resize)
    // 容器尺寸变化也要跟着重画:详情弹窗的宽度随视口变,挂载那一刻弹窗可能还在展开
    // 动画里,echarts.init 量到的宽度偏小、画出来挤在左边,而 window.resize 不会触发。
    // ResizeObserver 对"容器出现 / 变宽"都有效,弹窗与独立页两种情况都覆盖。
    if (typeof ResizeObserver !== 'undefined') {
      ro = new ResizeObserver(() => resize())
      ro.observe(el.value)
    }
  }
})
function resize() { chart?.resize() }
onBeforeUnmount(() => {
  window.removeEventListener('resize', resize)
  ro?.disconnect()
  ro = null
  chart?.dispose()
})
// 数据、节点序列、阈值或语言变化都要重画:canvas 上的系列名/轴名/阈值标注都是画上去的,
// 不重画就还是旧语言的文字(编辑监控改了阈值也要跟着走)。
watch(() => [props.data, props.agents, props.metric, props.speedUnit, props.threshold, locale.value], render, { deep: true })
</script>

<template>
  <div ref="el" style="width: 100%; height: 340px" />
</template>
