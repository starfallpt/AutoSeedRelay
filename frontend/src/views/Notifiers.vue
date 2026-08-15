<template>
  <div class="notifiers-page">
    <el-tabs v-model="activeTab">
      <!-- ============ 标签一：通知实例 ============ -->
      <el-tab-pane label="通知实例" name="instances">
        <el-card shadow="never">
          <div class="toolbar">
            <el-button type="primary" @click="openCreate">新增实例</el-button>
          </div>

          <el-table v-loading="notifierLoading" :data="notifiers" empty-text="暂无通知实例">
            <el-table-column prop="name" label="名称" min-width="140" />
            <el-table-column prop="type" label="类型" width="120" />
            <el-table-column label="状态" width="90" align="center">
              <template #default="{ row }">
                <el-tag :type="row.enabled ? 'success' : 'info'" size="small">
                  {{ row.enabled ? '启用' : '停用' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="200" fixed="right">
              <template #default="{ row }">
                <el-button size="small" @click="openEdit(row)">编辑</el-button>
                <el-button size="small" @click="testNotifier(row)">测试</el-button>
                <el-button size="small" type="danger" @click="deleteNotifier(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-card>
      </el-tab-pane>

      <!-- ============ 标签二：路由矩阵 ============ -->
      <el-tab-pane label="路由矩阵" name="matrix">
        <el-card shadow="never">
          <div class="toolbar">
            <el-button
              type="primary"
              :loading="matrixSaving"
              :disabled="notifiers.length === 0"
              @click="saveMatrix"
            >
              保存路由
            </el-button>
          </div>

          <el-alert
            v-if="notifiers.length === 0"
            title="暂无可用的通知器，请先在「通知实例」标签页创建实例。"
            type="info"
            :closable="false"
            show-icon
            class="matrix-hint"
          />

          <el-table v-loading="matrixLoading" :data="notifiers" border>
            <el-table-column prop="name" label="通知实例" min-width="160" fixed="left" />
            <el-table-column label="紧急（critical）" width="150" align="center">
              <template #default="{ row }">
                <el-switch
                  :model-value="isRouted(row.id, 'critical')"
                  @change="toggleRoute(row.id, 'critical', $event)"
                />
              </template>
            </el-table-column>
            <el-table-column label="警告（warning）" width="150" align="center">
              <template #default="{ row }">
                <el-switch
                  :model-value="isRouted(row.id, 'warning')"
                  @change="toggleRoute(row.id, 'warning', $event)"
                />
              </template>
            </el-table-column>
            <el-table-column label="信息（info）" width="150" align="center">
              <template #default="{ row }">
                <el-switch
                  :model-value="isRouted(row.id, 'info')"
                  @change="toggleRoute(row.id, 'info', $event)"
                />
              </template>
            </el-table-column>
          </el-table>
        </el-card>
      </el-tab-pane>
    </el-tabs>

    <!-- 实例编辑对话框 -->
    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑通知实例' : '新增通知实例'"
      width="560px"
      :close-on-click-modal="false"
    >
      <el-form label-width="90px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="通知实例名称" />
        </el-form-item>
        <el-form-item label="类型" required>
          <el-select v-model="form.type" style="width: 100%">
            <el-option v-for="t in TYPE_OPTIONS" :key="t" :label="t" :value="t" />
          </el-select>
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item label="配置">
          <div class="config-editor">
            <div class="config-hint">
              键名使用下划线（如 webhook_url、telegram_token、smtp_host），值可填写数字 / JSON。
            </div>
            <div v-for="(row, i) in form.configRows" :key="i" class="config-row">
              <el-input v-model="row.key" placeholder="键（如 webhook_url）" />
              <el-input v-model="row.value" placeholder="值" />
              <el-button link type="danger" @click="removeConfigRow(i)">删除</el-button>
            </div>
            <el-button size="small" @click="addConfigRow">添加配置项</el-button>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'
import type { Notifier, NotifierRoute, TestResult } from '../api/types'

const TYPE_OPTIONS = ['webhook', 'telegram', 'smtp', 'ntfy', 'gotify', 'serverchan', 'pushplus']

// ============ 实例列表 ============
const notifiers = ref<Notifier[]>([])
const notifierLoading = ref(false)

// ============ 实例表单 ============
interface ConfigRow {
  key: string
  value: string
}
interface NotifierForm {
  name: string
  type: string
  enabled: boolean
  configRows: ConfigRow[]
}
const dialogVisible = ref(false)
const submitting = ref(false)
const editingId = ref<number | null>(null)
const form = reactive<NotifierForm>({
  name: '',
  type: 'webhook',
  enabled: true,
  configRows: [],
})

// ============ 路由矩阵 ============
const matrixLoading = ref(false)
const matrixSaving = ref(false)
/** 已启用路由单元，键为 `${instance_id}|${tier}` */
const matrixState = reactive<Record<string, boolean>>({})

async function loadNotifiers(): Promise<void> {
  notifierLoading.value = true
  try {
    const { data } = await api.get<Notifier[]>('/notifiers')
    notifiers.value = data ?? []
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    notifierLoading.value = false
  }
}

async function loadMatrix(): Promise<void> {
  matrixLoading.value = true
  try {
    const { data } = await api.get<NotifierRoute[]>('/notifiers/routes')
    Object.keys(matrixState).forEach((k) => delete matrixState[k])
    for (const rt of data ?? []) {
      matrixState[`${rt.instance_id}|${rt.tier}`] = true
    }
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    matrixLoading.value = false
  }
}

// ============ 配置值解析 ============
function parseConfigValue(raw: string): unknown {
  const v = raw.trim()
  if (v === '') return ''
  if (v === 'true') return true
  if (v === 'false') return false
  if (/^-?\d+(\.\d+)?$/.test(v)) return Number(v)
  if (v.startsWith('{') || v.startsWith('[')) {
    try {
      return JSON.parse(v)
    } catch {
      /* fall through to string */
    }
  }
  return v
}

function displayValue(v: unknown): string {
  if (typeof v === 'string') return v
  if (v == null) return ''
  return JSON.stringify(v)
}

function buildConfigPayload(): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const row of form.configRows) {
    const key = row.key.trim()
    if (!key) continue
    out[key] = parseConfigValue(row.value)
  }
  return out
}

function resetForm(): void {
  editingId.value = null
  form.name = ''
  form.type = 'webhook'
  form.enabled = true
  form.configRows = []
}

function addConfigRow(): void {
  form.configRows.push({ key: '', value: '' })
}

function removeConfigRow(i: number): void {
  form.configRows.splice(i, 1)
}

function openCreate(): void {
  resetForm()
  dialogVisible.value = true
}

function openEdit(row: Notifier): void {
  editingId.value = row.id
  form.name = row.name
  form.type = row.type
  form.enabled = row.enabled
  const cfg = row.config ?? {}
  form.configRows = Object.entries(cfg).map(([key, value]) => ({
    key,
    value: displayValue(value),
  }))
  dialogVisible.value = true
}

async function handleSubmit(): Promise<void> {
  if (!form.name.trim()) {
    ElMessage.warning('请输入名称')
    return
  }
  submitting.value = true
  try {
    const payload = {
      name: form.name.trim(),
      type: form.type,
      config: buildConfigPayload(),
      enabled: form.enabled,
    }
    if (editingId.value) {
      await api.put(`/notifiers/${editingId.value}`, payload)
      ElMessage.success('通知实例已更新')
    } else {
      await api.post('/notifiers', payload)
      ElMessage.success('通知实例已创建')
    }
    dialogVisible.value = false
    await loadNotifiers()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    submitting.value = false
  }
}

async function testNotifier(row: Notifier): Promise<void> {
  try {
    const { data } = await api.post<TestResult>(`/notifiers/${row.id}/test`)
    if (data.ok) {
      ElMessage.success(data.message || '测试发送成功')
    } else {
      ElMessage.error(data.message || '测试发送失败')
    }
  } catch {
    // 错误由响应拦截器统一提示
  }
}

async function deleteNotifier(row: Notifier): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除通知实例「${row.name}」？`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.delete(`/notifiers/${row.id}`)
    ElMessage.success('已删除')
    await Promise.all([loadNotifiers(), loadMatrix()])
  } catch {
    // 错误由响应拦截器统一提示
  }
}

// ============ 路由矩阵 ============
function matrixKey(instanceId: number, tier: string): string {
  return `${instanceId}|${tier}`
}

function isRouted(instanceId: number, tier: 'critical' | 'warning' | 'info'): boolean {
  return matrixState[matrixKey(instanceId, tier)] === true
}

function toggleRoute(instanceId: number, tier: 'critical' | 'warning' | 'info', on: boolean): void {
  const key = matrixKey(instanceId, tier)
  if (on) {
    matrixState[key] = true
  } else {
    delete matrixState[key]
  }
}

async function saveMatrix(): Promise<void> {
  matrixSaving.value = true
  try {
    const routes: NotifierRoute[] = []
    for (const n of notifiers.value) {
      for (const tier of ['critical', 'warning', 'info'] as const) {
        if (matrixState[matrixKey(n.id, tier)] === true) {
          routes.push({ instance_id: n.id, tier })
        }
      }
    }
    await api.put('/notifiers/routes', routes)
    ElMessage.success('路由已保存')
    await loadMatrix()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    matrixSaving.value = false
  }
}

// ============ 初始化 ============
const activeTab = ref('instances')

onMounted(async () => {
  await Promise.all([loadNotifiers(), loadMatrix()])
})
</script>

<style scoped>
.notifiers-page {
  min-height: 100%;
}

.toolbar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: 12px;
}

.matrix-hint {
  margin-bottom: 12px;
}

.config-editor {
  width: 100%;
}

.config-hint {
  margin-bottom: 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.config-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.config-row .el-input {
  flex: 1;
}
</style>
