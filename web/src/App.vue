<script setup lang="ts">
import { startRealtime } from './api/realtime'
import { elementLocale } from './i18n'
startRealtime()
</script>

<template>
  <!-- Element Plus 组件的文案(日期选择器、下拉空态、表格默认空态等)跟随当前语言 -->
  <el-config-provider :locale="elementLocale">
    <router-view />
  </el-config-provider>
</template>
