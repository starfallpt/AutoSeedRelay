<template>
  <div class="qb-page">
    <el-card>
      <template #header>
        <div class="card-header">
          <span class="card-title">qB 实例</span>
          <el-button type="primary" @click="openCreate">新增 qB 实例</el-button>
        </div>
      </template>

      <el-table v-loading="loading" :data="list" empty-text="暂无 qB 实例">
        <el-table-column prop="name" label="名称" min-width="140" show-overflow-tooltip />
        <el-table-column label="地址" min-width="200" show-overflow-tooltip>
          <template #default="{ row }">{{ row.host }}:{{ row.port }}</template>
        </el-table-column>
        <el-table-column prop="username" label="用户名" width="120" show-overflow-tooltip />
        <el-table-column prop="priority" label="优先级" width="90" align="center" />
        <el-table-column label="启用" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" size="small">
              {{ row.enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="success" :loading="testingId === row.id" @click="onTest(row)">测试</el-button>
            <el-button link type="danger" @click="onDelete(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑 qB 实例' : '新增 qB 实例'"
      width="520px"
      @closed="onDialogClosed"
    >
      <el-form :model="form" label-width="90px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="请输入实例名称" />
        </el-form-item>
        <el-form-item label="主机" required>
          <el-input v-model="form.host" placeholder="例如 http://127.0.0.1" />
        </el-form-item>
        <el-form-item label="端口">
          <el-input-number v-model="form.port" :min="1" :max="65535" controls-position="right" />
        </el-form-item>
        <el-form-item label="用户名" required>
          <el-input v-model="form.username" placeholder="请输入用户名" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            :placeholder="passwordPlaceholder"
          />
        </el-form-item>
        <el-form-item label="优先级">
          <el-input-number v-model="form.priority" :min="0" controls-position="right" />
        </el-form-item>
        <el-form-item label="附加配置">
          <el-input
            v-model="form.extraText"
            type="textarea"
            :rows="3"
            placeholder='JSON 对象（可选），例如：{"download_dir":"/downloads"}'
          />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="onSave">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'
import type { JSONObject, QBInstance, QBInput, TestResult } from '../api/types'

const list = ref<QBInstance[]>([])
const loading = ref(false)
const saving = ref(false)
const dialogVisible = ref(false)
const editingId = ref<number | null>(null)
const testingId = ref<number | null>(null)
const passwordMasked = ref(false)

interface QbForm {
  name: string
  host: string
  port: number
  username: string
  password: string
  priority: number
  extraText: string
  enabled: boolean
}

const form = reactive<QbForm>({
  name: '',
  host: '',
  port: 8080,
  username: '',
  password: '',
  priority: 0,
  extraText: '',
  enabled: true,
})

const passwordPlaceholder = computed(() => (passwordMasked.value ? '保持不变' : '请输入密码'))

function parseJSON(text: string): { ok: true; value?: JSONObject | null } | { ok: false } {
  const t = text.trim()
  if (!t) return { ok: true, value: null }
  try {
    const parsed: unknown = JSON.parse(t)
    if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) {
      return { ok: true, value: parsed as JSONObject }
    }
    return { ok: false }
  } catch {
    return { ok: false }
  }
}

async function fetchList() {
  loading.value = true
  try {
    const { data } = await api.get<QBInstance[]>('/qb')
    list.value = data ?? []
  } catch {
    // 错误已由拦截器统一提示
  } finally {
    loading.value = false
  }
}

function resetForm() {
  form.name = ''
  form.host = ''
  form.port = 8080
  form.username = ''
  form.password = ''
  form.priority = 0
  form.extraText = ''
  form.enabled = true
  passwordMasked.value = false
}

function openCreate() {
  editingId.value = null
  resetForm()
  dialogVisible.value = true
}

function openEdit(row: QBInstance) {
  editingId.value = row.id
  form.name = row.name
  form.host = row.host
  form.port = row.port
  form.username = row.username
  form.password = ''
  form.priority = row.priority
  form.extraText = row.extra ? JSON.stringify(row.extra, null, 2) : ''
  form.enabled = row.enabled
  passwordMasked.value = row.password === '***'
  dialogVisible.value = true
}

function onDialogClosed() {
  editingId.value = null
  resetForm()
}

async function onSave() {
  if (!form.name.trim() || !form.host.trim() || !form.username.trim()) {
    ElMessage.warning('请填写名称、主机和用户名')
    return
  }
  const extra = parseJSON(form.extraText)
  if (!extra.ok) {
    ElMessage.error('附加配置必须是 JSON 对象')
    return
  }
  saving.value = true
  try {
    const payload: QBInput = {
      name: form.name.trim(),
      host: form.host.trim(),
      port: form.port,
      username: form.username.trim(),
      priority: form.priority,
      enabled: form.enabled,
      extra: extra.value ?? null,
    }
    if (editingId.value) {
      // 编辑：密码留空则省略该字段，保持后端原值
      if (form.password !== '') {
        payload.password = form.password
      }
      await api.put(`/qb/${editingId.value}`, payload)
      ElMessage.success('已保存')
    } else {
      payload.password = form.password
      await api.post('/qb', payload)
      ElMessage.success('已新增')
    }
    dialogVisible.value = false
    await fetchList()
  } catch {
    // 错误已由拦截器统一提示
  } finally {
    saving.value = false
  }
}

async function onTest(row: QBInstance) {
  testingId.value = row.id
  try {
    const { data } = await api.post<TestResult>(`/qb/${row.id}/test`)
    const latency = typeof data.latency_ms === 'number' ? `（${data.latency_ms} ms）` : ''
    const detail = data.message ? `：${data.message}` : ''
    if (data.ok) {
      ElMessage.success(`测试通过${latency}${detail}`)
    } else {
      ElMessage.error(`测试失败${latency}${detail}`)
    }
  } catch {
    // 错误已由拦截器统一提示
  } finally {
    testingId.value = null
  }
}

async function onDelete(row: QBInstance) {
  try {
    await ElMessageBox.confirm(`确定要删除 qB 实例「${row.name}」吗？`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return // 用户取消
  }
  try {
    await api.delete(`/qb/${row.id}`)
    ElMessage.success('已删除')
    await fetchList()
  } catch {
    // 错误已由拦截器统一提示
  }
}

onMounted(fetchList)
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.card-title {
  font-size: 15px;
  font-weight: 600;
  color: #303133;
}
</style>
