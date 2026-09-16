<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { http, TOKEN_KEY } from '../api/http'
import LocaleSwitch from '../components/LocaleSwitch.vue'

const { t } = useI18n()
const router = useRouter()
const mode = ref<'login' | 'init'>('login')
const form = ref({ username: '', password: '' })
const loading = ref(false)

// 进入登录页时探测是否已初始化管理员
http.get('/auth/status').then((data: any) => {
  if (!data.initialized) mode.value = 'init'
}).catch(() => {})

async function submit() {
  if (!form.value.username || form.value.password.length < 6) {
    ElMessage.warning(t('login.invalid'))
    return
  }
  loading.value = true
  try {
    const data: any = await http.post(mode.value === 'init' ? '/auth/init' : '/auth/login', form.value)
    localStorage.setItem(TOKEN_KEY, data.token)
    await router.push({ name: 'overview' })
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <!-- 登录页没有顶部栏,单独在右上角放一个语言切换:首次访问就能选语言 -->
    <div class="locale-corner"><LocaleSwitch /></div>
    <el-card class="login-card">
      <h2>UptimeMesh {{ t(mode === 'init' ? 'login.titleInit' : 'login.titleLogin') }}</h2>
      <p v-if="mode === 'init'" class="hint">{{ t('login.initHint') }}</p>
      <el-form @submit.prevent="submit">
        <el-form-item>
          <el-input v-model="form.username" :placeholder="t('login.username')" />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.password"
            type="password"
            show-password
            :placeholder="t('login.password')"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" style="width:100%" :loading="loading" @click="submit">
          {{ t(mode === 'init' ? 'login.submitInit' : 'login.submitLogin') }}
        </el-button>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.login-wrap {
  height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #f0f2f5;
}
.login-card { width: 380px; }
.hint { color: #909399; font-size: 13px; }
.locale-corner {
  position: fixed;
  top: 16px;
  right: 20px;
}
</style>
