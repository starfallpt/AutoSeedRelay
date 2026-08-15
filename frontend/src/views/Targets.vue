<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'
import type { JSONObject, Target, TargetInput, TestResult } from '../api/types'

// ============ 列表 ============
const loading = ref(false)
const targets = ref<Target[]>([])

// ============ 新增 / 编辑表单 ============
interface TargetForm {
  name: string
  base_url: string
  type: string
  announce: string
  passkey: string
  cookie: string
  api_token: string
  test_mode: boolean
  enabled: boolean
  fallback_category: string
  categoryText: string
  dimensionsText: string
  tagsText: string
}

const TYPE_OPTIONS = ['nexusphp', 'nexusphp_classic', 'mteam']

const dialogVisible = ref(false)
const submitting = ref(false)
const editingId = ref<number | null>(null)
const probingId = ref<number | null>(null)
const testingId = ref<number | null>(null)
/** 编辑时值为 '***' 的凭据键（留空表示不修改） */
const masked = reactive({ passkey: false, cookie: false, api_token: false })
const form = reactive<TargetForm>({
  name: '',
  base_url: '',
  type: 'nexusphp',
  announce: '',
  passkey: '',
  cookie: '',
  api_token: '',
  test_mode: false,
  enabled: true,
  fallback_category: '',
  categoryText: '',
  dimensionsText: '',
  tagsText: '',
})

// ============ 探测弹窗 ============
interface ProbeResponse {
  ok: boolean
  type?: string
  sections?: unknown[]
  categories?: unknown[]
  tags?: unknown[]
  codec_list?: unknown[]
}
const probeDialogVisible = ref(false)
const probeTargetName = ref('')
const probeResult = ref<ProbeResponse | null>(null)

/** 凭据提交处理：编辑时掩码留空 → '***'（保持后端原值）；否则原样提交 */
function credentialValue(raw: string, isMasked: boolean): string {
  const v = raw.trim()
  if (isMasked && v === '') return '***'
  return v
}

/** 解析 JSON 对象文本：空串→null；非法→失败 */
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

/** 拉取列表 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const { data } = await api.get<Target[]>('/targets')
    targets.value = data ?? []
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    loading.value = false
  }
}

/** 重置表单 */
function resetForm(): void {
  editingId.value = null
  masked.passkey = false
  masked.cookie = false
  masked.api_token = false
  form.name = ''
  form.base_url = ''
  form.type = 'nexusphp'
  form.announce = ''
  form.passkey = ''
  form.cookie = ''
  form.api_token = ''
  form.test_mode = false
  form.enabled = true
  form.fallback_category = ''
  form.categoryText = ''
  form.dimensionsText = ''
  form.tagsText = ''
}

/** 新增 */
function openCreate(): void {
  resetForm()
  dialogVisible.value = true
}

/** 编辑回填 */
function openEdit(row: Target): void {
  editingId.value = row.id
  masked.passkey = row.passkey === '***'
  masked.cookie = row.cookie === '***'
  masked.api_token = row.api_token === '***'
  form.name = row.name
  form.base_url = row.base_url
  form.type = row.type
  form.announce = row.announce
  form.passkey = masked.passkey ? '' : row.passkey
  form.cookie = masked.cookie ? '' : row.cookie
  form.api_token = masked.api_token ? '' : row.api_token
  form.test_mode = row.test_mode
  form.enabled = row.status === 'active'
  form.fallback_category = row.fallback_category != null ? String(row.fallback_category) : ''
  form.categoryText = row.category_overrides ? JSON.stringify(row.category_overrides, null, 2) : ''
  form.dimensionsText = row.dimension_overrides ? JSON.stringify(row.dimension_overrides, null, 2) : ''
  form.tagsText = row.tags_map ? JSON.stringify(row.tags_map, null, 2) : ''
  dialogVisible.value = true
}

/** 提交（新增 / 更新） */
async function handleSubmit(): Promise<void> {
  if (!form.name.trim()) {
    ElMessage.warning('请输入名称')
    return
  }
  if (!form.base_url.trim()) {
    ElMessage.warning('请输入站点地址')
    return
  }
  const category = parseJSON(form.categoryText)
  const dimensions = parseJSON(form.dimensionsText)
  const tags = parseJSON(form.tagsText)
  if (!category.ok || !dimensions.ok || !tags.ok) {
    ElMessage.error('覆盖映射 / 维度覆盖 / 标签映射必须是 JSON 对象')
    return
  }

  submitting.value = true
  try {
    const payload: TargetInput = {
      name: form.name.trim(),
      type: form.type.trim(),
      base_url: form.base_url.trim(),
      status: form.enabled ? 'active' : 'paused',
      test_mode: form.test_mode,
      passkey: credentialValue(form.passkey, masked.passkey),
      cookie: credentialValue(form.cookie, masked.cookie),
      api_token: credentialValue(form.api_token, masked.api_token),
      category_overrides: category.value ?? null,
      dimension_overrides: dimensions.value ?? null,
      tags_map: tags.value ?? null,
    }
    const announce = form.announce.trim()
    if (announce) payload.announce = announce
    const fb = form.fallback_category.trim()
    if (fb) {
      const n = Number(fb)
      if (Number.isInteger(n)) payload.fallback_category = n
    }

    if (editingId.value) {
      await api.put(`/targets/${editingId.value}`, payload)
      ElMessage.success('目标站已更新')
    } else {
      await api.post('/targets', payload)
      ElMessage.success('目标站已创建')
    }
    dialogVisible.value = false
    await load()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    submitting.value = false
  }
}

/** 探测 */
async function handleProbe(row: Target): Promise<void> {
  probingId.value = row.id
  try {
    const { data } = await api.post<ProbeResponse>(`/targets/${row.id}/probe`)
    probeResult.value = data
    probeTargetName.value = row.name
    probeDialogVisible.value = true
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    probingId.value = null
  }
}

/** 行内测试 */
async function handleTest(row: Target): Promise<void> {
  testingId.value = row.id
  try {
    const { data } = await api.post<TestResult>(`/targets/${row.id}/test`)
    if (data.ok) {
      ElMessage.success(data.message || '连接测试成功')
    } else {
      ElMessage.error(data.message || '连接测试失败')
    }
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    testingId.value = null
  }
}

/** 删除 */
async function handleDelete(row: Target): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除目标站「${row.name}」？删除后不可恢复。`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.delete(`/targets/${row.id}`)
    ElMessage.success('目标站已删除')
    await load()
  } catch {
    // 错误由响应拦截器统一提示
  }
}

onMounted(load)
</script>

<template>
  <section class="page">
    <el-card shadow="never" class="card">
      <template #header>
        <div class="card-header">
          <span class="card-title">目标站</span>
          <el-button type="primary" @click="openCreate">新增目标站</el-button>
        </div>
      </template>

      <el-table
        v-loading="loading"
        :data="targets"
        empty-text="暂无目标站，点击右上角「新增目标站」开始添加"
      >
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="base_url" label="地址" min-width="200" show-overflow-tooltip />
        <el-table-column prop="type" label="类型" width="140" />
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }">
            <el-tag :type="row.status === 'active' ? 'success' : 'info'" size="small">
              {{ row.status === 'active' ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="测试模式" width="90" align="center">
          <template #default="{ row }">
            <el-tag v-if="row.test_mode" type="warning" size="small">测试</el-tag>
            <span v-else>-</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="240" fixed="right">
          <template #default="{ row }">
            <el-button
              link
              type="primary"
              size="small"
              :loading="probingId === row.id"
              @click="handleProbe(row)"
            >
              探测
            </el-button>
            <el-button
              link
              type="primary"
              size="small"
              :loading="testingId === row.id"
              @click="handleTest(row)"
            >
              测试
            </el-button>
            <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" size="small" @click="handleDelete(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑目标站' : '新增目标站'"
      width="620px"
      :close-on-click-modal="false"
    >
      <el-form label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="目标站名称" maxlength="100" />
        </el-form-item>
        <el-form-item label="站点地址" required>
          <el-input v-model="form.base_url" placeholder="目标站地址，如 https://example.com" />
        </el-form-item>
        <el-form-item label="类型" required>
          <el-select v-model="form.type" style="width: 100%">
            <el-option v-for="t in TYPE_OPTIONS" :key="t" :label="t" :value="t" />
          </el-select>
        </el-form-item>
        <el-form-item label="Announce">
          <el-input v-model="form.announce" placeholder="announce 地址（可选）" />
        </el-form-item>
        <el-form-item label="Passkey">
          <el-input
            v-model="form.passkey"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="masked.passkey ? '已保存，留空表示不修改' : '请输入 Passkey（可选）'"
          />
        </el-form-item>
        <el-form-item label="Cookie">
          <el-input
            v-model="form.cookie"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="masked.cookie ? '已保存，留空表示不修改' : '请输入 Cookie（可选）'"
          />
        </el-form-item>
        <el-form-item label="API Token">
          <el-input
            v-model="form.api_token"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="masked.api_token ? '已保存，留空表示不修改' : '请输入 API Token（可选）'"
          />
        </el-form-item>
        <el-form-item label="测试模式">
          <el-switch v-model="form.test_mode" />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item label="回退分类 ID">
          <el-input v-model="form.fallback_category" placeholder="可选，如 401" />
        </el-form-item>
        <el-form-item label="分类覆盖">
          <el-input
            v-model="form.categoryText"
            type="textarea"
            :rows="4"
            placeholder='JSON 对象，例如：{"movie": 401}'
          />
        </el-form-item>
        <el-form-item label="维度覆盖">
          <el-input
            v-model="form.dimensionsText"
            type="textarea"
            :rows="4"
            placeholder='JSON 对象，例如：{"resolution": true}'
          />
        </el-form-item>
        <el-form-item label="标签映射">
          <el-input
            v-model="form.tagsText"
            type="textarea"
            :rows="4"
            placeholder='JSON 对象，例如：{"国语": "1"}'
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
      </template>
    </el-dialog>

    <el-dialog
      v-model="probeDialogVisible"
      :title="`探测结果：${probeTargetName}`"
      width="560px"
    >
      <div v-if="probeResult">
        <el-alert
          :type="probeResult.ok ? 'success' : 'error'"
          :title="probeResult.ok ? '探测成功' : '探测失败'"
          :closable="false"
          show-icon
        />
        <div class="probe-block">
          <div class="probe-label">类型</div>
          <div>{{ probeResult.type || '—' }}</div>
        </div>
        <div class="probe-block">
          <div class="probe-label">分类</div>
          <pre class="probe-json">{{ JSON.stringify(probeResult.categories ?? [], null, 2) }}</pre>
        </div>
        <div class="probe-block">
          <div class="probe-label">标签</div>
          <pre class="probe-json">{{ JSON.stringify(probeResult.tags ?? [], null, 2) }}</pre>
        </div>
      </div>
      <template #footer>
        <el-button type="primary" @click="probeDialogVisible = false">知道了</el-button>
      </template>
    </el-dialog>
  </section>
</template>

<style scoped>
.page {
  min-height: 100%;
}

.card {
  border-radius: 8px;
}

.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.card-title {
  font-size: 16px;
  font-weight: 600;
}

.probe-block {
  margin-top: 16px;
}

.probe-label {
  margin-bottom: 8px;
  font-size: 13px;
  color: var(--el-text-color-regular);
}

.probe-json {
  margin: 0;
  padding: 12px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.6;
  overflow: auto;
  max-height: 200px;
}
</style>
