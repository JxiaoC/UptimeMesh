<script setup lang="ts">
import { computed } from 'vue'
import { LANGUAGES, setLocale, useLocale } from '../i18n'

/**
 * 语言切换:放在后台顶部栏右上角(登录页也放一个,便于首次访问时切换)。
 * 显示当前语言的自称(简体中文 / English),下拉里标出当前项为禁用态。
 */
const { locale } = useLocale()
const current = computed(() => LANGUAGES.find((l) => l.code === locale.value) ?? LANGUAGES[0])

// el-dropdown 的 command 是宽类型(string | number | object),这里收窄后再切。
function onSelect(code: unknown) {
  if (code === 'zh-CN' || code === 'en-US') setLocale(code)
}
</script>

<template>
  <el-dropdown trigger="click" @command="onSelect">
    <span class="locale-switch" :title="`Language: ${current.label}`">
      {{ current.label }}<i class="caret" />
    </span>
    <template #dropdown>
      <el-dropdown-menu>
        <el-dropdown-item
          v-for="l in LANGUAGES"
          :key="l.code"
          :command="l.code"
          :disabled="l.code === locale"
        >
          {{ l.label }}
        </el-dropdown-item>
      </el-dropdown-menu>
    </template>
  </el-dropdown>
</template>

<style scoped>
.locale-switch {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
  font-size: 13px;
  color: #606266;
  outline: none;
}
.locale-switch:hover {
  color: var(--el-color-primary);
}
/* 下拉箭头用 CSS 画:不引 @element-plus/icons-vue(它只是 element-plus 的传递依赖) */
.caret {
  width: 0;
  height: 0;
  border-left: 4px solid transparent;
  border-right: 4px solid transparent;
  border-top: 4px solid currentColor;
}
</style>
