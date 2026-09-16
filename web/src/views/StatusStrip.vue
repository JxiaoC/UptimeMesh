<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatSpeed } from '../utils/speed'

interface RoundCell {
  // 色块口径由后端 webhub.RoundCellStatus 统一给出:
  //   up      达标轮(绿)
  //   breach  破线轮但连续破线还没攒够轮数(黄,预警)
  //   down    已判 DOWN 期间的破线轮(红,故障)
  //   unknown 无有效样本轮(灰)
  status: 'up' | 'breach' | 'down' | 'unknown'
  scheduledAt: string
  successRate: number
  // speedKbps 仅下载速度监控有值(本轮平均速度,KB/s):悬停提示显示速度而不是成功率。
  speedKbps?: number
}

const props = defineProps<{ rounds: RoundCell[]; speedUnit?: string }>()

const { t } = useI18n()

// 色块口径与列表页图例共用 status.* 词条;函数在渲染/提示期调用,切语言即重新求值。
const label = (s: RoundCell['status']) =>
  s === 'up' ? t('status.up')
    : s === 'breach' ? t('status.breach')
      : s === 'down' ? t('status.down')
        : t('status.noSample')

// 下载速度监控的判定值是速度,悬停要显示本轮平均速度(单位按监控配置换算)。
function tip(r: RoundCell) {
  const time = r.scheduledAt || t('common.none')
  if (props.speedUnit) {
    return t('status.roundTipSpeed', {
      time, label: label(r.status), speed: formatSpeed(r.speedKbps, props.speedUnit),
    })
  }
  return t('status.roundTip', { time, label: label(r.status), rate: r.successRate })
}

// 单实例 tooltip:virtual-ref 指向当前悬停色块,避免每根柱子各挂一个组件实例
// (100 根 × 多行监控会生成上千个 tooltip)。
const hovered = ref<HTMLElement | null>(null)
const activeIndex = ref<number | null>(null)

function onEnter(el: EventTarget | null, i: number) {
  hovered.value = el as HTMLElement
  activeIndex.value = i
}
function onLeave() {
  hovered.value = null
  activeIndex.value = null
}
const activeCell = () =>
  activeIndex.value === null ? null : props.rounds[activeIndex.value] ?? null
</script>

<template>
  <div v-if="rounds.length" class="strip" @mouseleave="onLeave">
    <span
      v-for="(r, i) in rounds"
      :key="i"
      class="bar"
      :class="[r.status, { active: activeIndex === i }]"
      @mouseenter="onEnter($event.currentTarget, i)"
    />
    <el-tooltip
      :visible="activeIndex !== null"
      :virtual-ref="hovered"
      virtual-triggering
      placement="top"
      :show-arrow="false"
      :offset="6"
    >
      <template #content>
        <span v-if="activeCell()">{{ tip(activeCell()!) }}</span>
      </template>
    </el-tooltip>
  </div>
  <span v-else class="empty">{{ t('status.noRounds') }}</span>
</template>

<style scoped>
.strip {
  display: flex;
  justify-content: flex-end; /* 最右为最新轮次 */
  align-items: center;
  gap: 1px;
  min-height: 20px;
}
.bar {
  position: relative;
  display: block;
  width: 6px;
  height: 20px;
  border-radius: 2px;
  flex: none;
  transition: transform 0.08s ease, box-shadow 0.08s ease, filter 0.08s ease;
}
.bar.up { background: #67c23a; }
/* 破线但还没判 DOWN:黄色预警 */
.bar.breach { background: #e6a23c; }
/* 已判 DOWN:红色故障 */
.bar.down { background: #f56c6c; }
.bar.unknown { background: #c0c4cc; }
/* 悬停高亮:轻微放大并描边,放大期间压过相邻色块 */
.bar.active {
  transform: scaleX(1.6) scaleY(1.12);
  box-shadow: 0 0 0 1px #fff, 0 0 4px rgba(0, 0, 0, 0.3);
  filter: brightness(1.08);
  z-index: 1;
}
.empty { color: #909399; font-size: 12px; }
</style>
