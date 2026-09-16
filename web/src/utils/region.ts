// 地域码(ISO 3166-1 alpha-2)→ 名称与国旗,名称按当前界面语言给。
// 国旗取自 flag-icons(CSS 类 fi fi-<小写码>),样式在 main.ts 全局引入。
//
// 政治正确约定:港澳台固定显示为「中国香港 / 中国澳门 / 中国台湾」(英文写作
// Hong Kong, China / Macao, China / Taiwan, China)。不能只靠 Intl.DisplayNames:
// 它在 zh 下会给出「中国香港特别行政区」与「台湾」,违反本项目的约定,所以这几条走
// 显式覆盖表;其余地域码交给 Intl(浏览器自带 CLDR 数据,免维护上百条译名),
// Intl 不可用或认不出该码时回落到下面的内置中文表。
import { locale as currentLocale } from '../i18n'

const REGION_NAMES: Record<string, string> = {
  CN: '中国', HK: '中国香港', MO: '中国澳门', TW: '中国台湾',
  JP: '日本', KR: '韩国', KP: '朝鲜', MN: '蒙古',
  SG: '新加坡', MY: '马来西亚', TH: '泰国', VN: '越南', KH: '柬埔寨', LA: '老挝',
  MM: '缅甸', PH: '菲律宾', ID: '印度尼西亚', BN: '文莱', TL: '东帝汶', NP: '尼泊尔',
  IN: '印度', PK: '巴基斯坦', BD: '孟加拉国', LK: '斯里兰卡', MV: '马尔代夫',
  KZ: '哈萨克斯坦', UZ: '乌兹别克斯坦', KG: '吉尔吉斯斯坦', TJ: '塔吉克斯坦', TM: '土库曼斯坦',
  AE: '阿联酋', SA: '沙特阿拉伯', QA: '卡塔尔', KW: '科威特', BH: '巴林', OM: '阿曼',
  YE: '也门', IL: '以色列', IQ: '伊拉克', IR: '伊朗', TR: '土耳其', JO: '约旦', LB: '黎巴嫩',
  SY: '叙利亚', GE: '格鲁吉亚', AM: '亚美尼亚', AZ: '阿塞拜疆', CY: '塞浦路斯',
  RU: '俄罗斯', UA: '乌克兰', BY: '白俄罗斯', MD: '摩尔多瓦', PL: '波兰',
  DE: '德国', FR: '法国', GB: '英国', IT: '意大利', ES: '西班牙', PT: '葡萄牙',
  NL: '荷兰', BE: '比利时', LU: '卢森堡', IE: '爱尔兰', IS: '冰岛', DK: '丹麦',
  SE: '瑞典', NO: '挪威', FI: '芬兰', CH: '瑞士', AT: '奥地利', CZ: '捷克',
  SK: '斯洛伐克', HU: '匈牙利', RO: '罗马尼亚', BG: '保加利亚', RS: '塞尔维亚',
  HR: '克罗地亚', SI: '斯洛文尼亚', BA: '波黑', ME: '黑山', MK: '北马其顿',
  AL: '阿尔巴尼亚', GR: '希腊', MT: '马耳他', EE: '爱沙尼亚', LV: '拉脱维亚', LT: '立陶宛',
  US: '美国', CA: '加拿大', MX: '墨西哥', CU: '古巴', JM: '牙买加', HT: '海地',
  DO: '多米尼加', GT: '危地马拉', HN: '洪都拉斯', SV: '萨尔瓦多', NI: '尼加拉瓜',
  CR: '哥斯达黎加', PA: '巴拿马', CO: '哥伦比亚', VE: '委内瑞拉', EC: '厄瓜多尔',
  PE: '秘鲁', BO: '玻利维亚', BR: '巴西', CL: '智利', AR: '阿根廷', UY: '乌拉圭',
  PY: '巴拉圭', GY: '圭亚那', SR: '苏里南',
  AU: '澳大利亚', NZ: '新西兰', FJ: '斐济', PG: '巴布亚新几内亚',
  EG: '埃及', LY: '利比亚', TN: '突尼斯', DZ: '阿尔及利亚', MA: '摩洛哥',
  SD: '苏丹', SS: '南苏丹', ET: '埃塞俄比亚', KE: '肯尼亚', TZ: '坦桑尼亚',
  UG: '乌干达', RW: '卢旺达', NG: '尼日利亚', GH: '加纳', CI: '科特迪瓦',
  SN: '塞内加尔', CM: '喀麦隆', CD: '刚果(金)', CG: '刚果(布)', AO: '安哥拉',
  ZA: '南非', ZW: '津巴布韦', ZM: '赞比亚', MZ: '莫桑比克', NA: '纳米比亚',
  BW: '博茨瓦纳', MG: '马达加斯加', MU: '毛里求斯',
}

const KNOWN_CODES = new Set(Object.keys(REGION_NAMES))

// 各语言下的固定写法:港澳台是政治约定,不接受 Intl 的默认输出。
const REGION_OVERRIDES: Record<string, Record<string, string>> = {
  'zh-CN': { CN: '中国', HK: '中国香港', MO: '中国澳门', TW: '中国台湾' },
  'en-US': { CN: 'China', HK: 'Hong Kong, China', MO: 'Macao, China', TW: 'Taiwan, China' },
}

// Intl.DisplayNames 构造有开销(地域下拉一次要查上百个码),按语言缓存实例。
const displayNamesCache = new Map<string, Intl.DisplayNames | null>()

function displayNames(loc: string): Intl.DisplayNames | null {
  const cached = displayNamesCache.get(loc)
  if (cached !== undefined) return cached
  let dn: Intl.DisplayNames | null = null
  try {
    dn = typeof Intl !== 'undefined' && 'DisplayNames' in Intl
      ? new Intl.DisplayNames([loc], { type: 'region' })
      : null
  } catch {
    dn = null // 老浏览器没有该 API:回落内置中文表
  }
  displayNamesCache.set(loc, dn)
  return dn
}

/**
 * 生效地域码 → 当前语言的名称;未收录的码原样回显(如 "XX")。
 * 内部读响应式的当前语言,所以模板里直接调用即随语言切换重新渲染。
 */
export function regionName(code: string): string {
  if (!code) return ''
  const c = code.toUpperCase()
  // ZZ 是「未知地区」的技术占位码:回显原码,由调用方决定怎么展示。
  if (c === 'ZZ') return c
  const override = REGION_OVERRIDES[currentLocale.value]?.[c]
  if (override) return override
  const viaIntl = displayNames(currentLocale.value)?.of(c)
  // Intl 认不出时会原样回显代码,这时才回落内置表。
  if (viaIntl && viaIntl !== c) return viaIntl
  return REGION_NAMES[c] || c
}

/** flag-icons 类名;无法识别的码返回空串(不渲染旗子)。 */
export function flagClass(code: string): string {
  const c = (code || '').toUpperCase()
  return KNOWN_CODES.has(c) ? `fi fi-${c.toLowerCase()}` : ''
}

// flagUrl 取该地域码对应的旗帜图片地址:ECharts 图例画在 canvas 上,用不了 CSS 类,
// 故从已全局加载的 flag-icons 样式里回读 background-image(生产构建下通常是内联 data URI)。
const flagUrlCache = new Map<string, string>()

export function flagUrl(code: string): string {
  const c = (code || '').toUpperCase()
  if (!KNOWN_CODES.has(c)) return ''
  const cached = flagUrlCache.get(c)
  if (cached !== undefined) return cached
  let url = ''
  if (typeof document !== 'undefined') {
    const probe = document.createElement('span')
    probe.className = `fi fi-${c.toLowerCase()}`
    // 不能用 display:none(取不到 background-image),移出视口即可。
    probe.style.cssText = 'position:absolute;left:-9999px;top:0;width:1px;height:1px'
    document.body.appendChild(probe)
    const bg = getComputedStyle(probe).backgroundImage
    probe.remove()
    // 值形如 url("data:image/svg+xml,..."):数据里可能带单引号,故只剥外层包裹再配对引号。
    const m = /^url\((.*)\)$/.exec(bg.trim())
    url = m ? m[1].trim().replace(/^(['"])(.*)\1$/, '$2') : ''
  }
  flagUrlCache.set(c, url)
  return url
}

/**
 * regionOptions 手动编辑弹窗的可选项(按当前语言的名字排序);空串 = 恢复按 IP 自动解析。
 * 每次调用要对上百项排序,组件里请用 computed 缓存(见 AgentsView)。
 */
export function regionOptions(): { code: string; name: string }[] {
  const collator = new Intl.Collator(currentLocale.value)
  return Object.keys(REGION_NAMES)
    .map((code) => ({ code, name: regionName(code) }))
    .sort((a, b) => collator.compare(a.name, b.name))
}
