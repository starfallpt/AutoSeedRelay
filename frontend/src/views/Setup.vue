<template>
  <div class="setup-page">
    <el-card class="setup-card" shadow="always">
      <div class="setup-header">
        <h1>AutoSeedRelay</h1>
        <p>首次使用，请设置管理员密码完成初始化</p>
      </div>

      <el-form :model="form" @submit.prevent="onSubmit">
        <el-form-item>
          <el-input
            v-model="form.password"
            type="password"
            placeholder="设置密码（至少 6 位）"
            show-password
            @keyup.enter="onSubmit"
          />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.confirm"
            type="password"
            placeholder="确认密码"
            show-password
            @keyup.enter="onSubmit"
          />
        </el-form-item>
        <el-button
          type="primary"
          class="setup-button"
          :loading="loading"
          @click="onSubmit"
        >
          完成初始化
        </el-button>
      </el-form>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'

const router = useRouter()
const auth = useAuthStore()

const form = reactive({ password: '', confirm: '' })
const loading = ref(false)

async function onSubmit() {
  if (form.password.length < 6) {
    ElMessage.warning('密码至少 6 位')
    return
  }
  if (form.password !== form.confirm) {
    ElMessage.warning('两次输入的密码不一致')
    return
  }

  loading.value = true
  try {
    await auth.setup(form.password)
    ElMessage.success('初始化完成')
    router.push('/')
  } catch {
    // 错误已由响应拦截器统一提示
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.setup-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1f2d3d 0%, #2d5a8e 100%);
}

.setup-card {
  width: 400px;
  border-radius: 12px;
  box-shadow: 0 8px 32px rgba(0, 0, 0, 0.25);
}

.setup-header {
  text-align: center;
  margin-bottom: 20px;
}

.setup-header h1 {
  margin: 0;
  font-size: 26px;
  background: linear-gradient(90deg, #409eff, #67c23a);
  -webkit-background-clip: text;
  background-clip: text;
  color: transparent;
}

.setup-header p {
  margin: 6px 0 0;
  color: #909399;
  font-size: 14px;
}

.setup-button {
  width: 100%;
}
</style>
