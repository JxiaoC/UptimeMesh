#!/usr/bin/env node
// i18n 残留检查:web/src 下的「面向用户的文案」不允许硬编码中文 —— 必须走 t() 词条。
//
// 用法:cd web && npm run i18n:check   (发现残留时 exit 1)
//
// 判定方式刻意做得笨一点、稳一点:
//   1. 先剥掉注释(块注释 / HTML 注释 / 行首或空白后的行注释),注释里写中文是允许的
//      —— 本仓库的注释就是中文的,而且注释不是给用户看的文案。
//   2. 再逐行找中日韩统一表意文字。模板里的裸文本、字符串字面量、属性值都会被抓到。
//   3. 豁免:i18n 词条目录(中文文案本来就存在那儿)与 utils/region.ts(内置中文地域名兜底表)。
//   4. 个别确实不该翻译的地方(语言自称等)可在行内写 `i18n-exempt` 放行。
//
// 为什么不做进 `npm run build`:它是静态扫描,误报会直接卡住发布;按 AGENTS.md
// 的约定在提交前手动跑一次即可(词条 key 对齐另有 vue-tsc 在 build 里把关)。
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const WEB_ROOT = fileURLToPath(new URL('..', import.meta.url))
const SRC_DIR = join(WEB_ROOT, 'src')

// 豁免路径(相对 web/):词条目录与地域名表。
const EXEMPT_PREFIXES = ['src/i18n', 'src/utils/region.ts']

const CJK = /[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]/

/** 收集 src 下的 .ts / .vue 文件(排除 i18n 词条目录)。 */
function collect(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    const rel = relative(WEB_ROOT, full).split('\\').join('/')
    if (EXEMPT_PREFIXES.some((p) => rel === p || rel.startsWith(p + '/'))) continue
    if (statSync(full).isDirectory()) out.push(...collect(full))
    else if (/\.(ts|vue)$/.test(name)) out.push(full)
  }
  return out
}

/** 把注释内容替换成等长空白(保留换行),这样行号与列位置都不会漂。 */
function stripComments(src) {
  const blank = (m) => m.replace(/[^\n]/g, ' ')
  return src
    .replace(/<!--[\s\S]*?-->/g, blank) // HTML 注释
    .replace(/\/\*[\s\S]*?\*\//g, blank) // 块注释
    // 行注释:只认「行首或空白/分隔符之后」的 //,免得把 https:// 这类地址当成注释。
    .replace(/(^|[\s;{(,[])\/\/[^\n]*/gm, (m, prefix) => prefix + ' '.repeat(m.length - prefix.length))
}

const problems = []
for (const file of collect(SRC_DIR)) {
  const rel = relative(WEB_ROOT, file).split('\\').join('/')
  const stripped = stripComments(readFileSync(file, 'utf8'))
  stripped.split('\n').forEach((line, i) => {
    if (!CJK.test(line)) return
    if (line.includes('i18n-exempt')) return
    problems.push(`${rel}:${i + 1}: ${line.trim()}`)
  })
}

if (problems.length) {
  console.error('发现未国际化的中文文案(请改用 t("...") 并补 zh-CN / en-US 词条):')
  for (const p of problems) console.error('  ' + p)
  console.error(`\n共 ${problems.length} 处。确实不该翻译的地方可在行内加 i18n-exempt 放行。`)
  process.exit(1)
}
console.log('i18n 检查通过:web/src 下没有未国际化的中文文案。')
