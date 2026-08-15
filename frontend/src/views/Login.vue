<template>
  <div class="login-page">
    <el-card class="login-card" shadow="always">
      <div class="login-header">
        <h1>AutoSeedRelay</h1>
        <p>PT 辅种平台</p>
      </div>

      <el-form :model="form" @keyup.enter="onSubmit">
        <el-form-item>
          <el-input v-model="form.username" placeholder="账号" clearable />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.password"
            type="password"
            placeholder="密码"
            show-password
          />
        </el-form-item>
        <el-button
          type="primary"
          class="login-button"
          :loading="loading"
          @click="onSubmit"
        >
          登录
        </el-button>
      </el-form>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import axios from 'axios'
import api from '../api'
import { useAuthStore } from '../stores/auth'

const router = useRouter()
const auth = useAuthStore()

const form = reactive({
  username: '',
  password: '',
})

const loading = ref(false)

async function onSubmit() {
  if (!form.username || !form.password) {
    ElMessage.warning('请输入账号和密码')
    return
  }

  loading.value = true
  try {
    const { data } = await api.post('/auth/login', {
      username: form.username,
      password: form.password,
    })
    // 占位：假定后端返回 token / session 字段，后续换 cookie session。
    auth.setToken(data?.token ?? data?.session_id ?? 'placeholder')
    ElMessage.success('登录成功')
    router.push('/')
  } catch (error) {
    let msg = '登录失败'
    if (axios.isAxiosError(error)) {
      const data = error.response?.data as
        | { error?: string; message?: string }
        | undefined
      msg = data?.error ?? data?.message ?? error.message ?? msg
    }
    ElMessage.error(msg)
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1f2d3d 0%, #2d5a8e 100%);
}

.login-card {
  width: 360px;
  border-radius: 12px;
  box-shadow: 0 8px 32px rgba(0, 0, 0, 0.25);
}

.login-header {
  text-align: center;
  margin-bottom: 20px;
}

.login-header h1 {
  margin: 0;
  font-size: 26px;
  background: linear-gradient(90deg, #409eff, #67c23a);
  -webkit-background-clip: text;
  background-clip: text;
  color: transparent;
}

.login-header p {
  margin: 6px 0 0;
  color: #909399;
  font-size: 14px;
}

.login-button {
  width: 100%;
}
</style>
