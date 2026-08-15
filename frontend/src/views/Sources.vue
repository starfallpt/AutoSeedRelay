<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'
import type { Source, SourceInput, TestResult } from '../api/types'

// ============ 列表 ============
const loading = ref(false)
const sources = ref<Source[]>([])

// ============ 新增 / 编辑表单 ============
interface SourceForm {
  name: string
  base_url: string
  rss_url: string
  role: string
  cookie: string
  passkey: string
  api_token: string
  enabled: boolean
}

const dialogVisible = ref(false)
const submitting = ref(false)
const editingId = ref<number | null>(null)
const testingId = ref<number | null>(null)
/** 编辑时值为 '***' 的凭据键（留空表示不修改） */
const masked = reactive({ cookie: false, passkey: false, api_token: false })
const form = reactive<SourceForm>({
  name: '',
  base_url: '',
  rss_url: '',
  role: 'source',
  cookie: '',
  passkey: '',
  api_token: '',
  enabled: true,
})

/** 凭据提交处理：编辑时掩码留空 → '***'（保持后端原值）；否则原样提交 */
function credentialValue(raw: string, isMasked: boolean): string {
  const v = raw.trim()
  if (isMasked && v === '') return '***'
  return v
}

function statusValue(enabled: boolean): string {
  return enabled ? 'active' : 'paused'
}

/** 拉取列表 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const { data } = await api.get<Source[]>('/sources')
    sources.value = data ?? []
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    loading.value = false
  }
}

/** 重置表单 */
function resetForm(): void {
  editingId.value = null
  masked.cookie = false
  masked.passkey = false
  masked.api_token = false
  form.name = ''
  form.base_url = ''
  form.rss_url = ''
  form.role = 'source'
  form.cookie = ''
  form.passkey = ''
  form.api_token = ''
  form.enabled = true
}

/** 新增 */
function openCreate(): void {
  resetForm()
  dialogVisible.value = true
}

/** 编辑回填 */
function openEdit(row: Source): void {
  editingId.value = row.id
  masked.cookie = row.cookie === '***'
  masked.passkey = row.passkey === '***'
  masked.api_token = row.api_token === '***'
  form.name = row.name
  form.base_url = row.base_url
  form.rss_url = row.rss_url
  form.role = row.role
  form.cookie = masked.cookie ? '' : row.cookie
  form.passkey = masked.passkey ? '' : row.passkey
  form.api_token = masked.api_token ? '' : row.api_token
  form.enabled = row.status === 'active'
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

  submitting.value = true
  try {
    const payload: SourceInput = {
      name: form.name.trim(),
      base_url: form.base_url.trim(),
      role: form.role.trim() || 'source',
      status: statusValue(form.enabled),
      cookie: credentialValue(form.cookie, masked.cookie),
      passkey: credentialValue(form.passkey, masked.passkey),
      api_token: credentialValue(form.api_token, masked.api_token),
    }
    const rss = form.rss_url.trim()
    if (rss) payload.rss_url = rss

    if (editingId.value) {
      await api.put(`/sources/${editingId.value}`, payload)
      ElMessage.success('站点源已更新')
    } else {
      await api.post('/sources', payload)
      ElMessage.success('站点源已创建')
    }
    dialogVisible.value = false
    await load()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    submitting.value = false
  }
}

/** 行内连接测试 */
async function handleTest(row: Source): Promise<void> {
  testingId.value = row.id
  try {
    const { data } = await api.post<TestResult>(`/sources/${row.id}/test`)
    if (data.ok) {
      ElMessage.success(data.message || '连接测试成功')
    } else {
      ElMessage.error(data.message || '连接测试失败')
    }
    await load()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    testingId.value = null
  }
}

/** 删除 */
async function handleDelete(row: Source): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除站点源「${row.name}」？删除后不可恢复。`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.delete(`/sources/${row.id}`)
    ElMessage.success('站点源已删除')
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
          <span class="card-title">站点源</span>
          <el-button type="primary" @click="openCreate">新增站点源</el-button>
        </div>
      </template>

      <el-table
        v-loading="loading"
        :data="sources"
        empty-text="暂无站点源，点击右上角「新增站点源」开始添加"
      >
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="base_url" label="地址" min-width="200" show-overflow-tooltip />
        <el-table-column prop="role" label="角色" width="100" />
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }">
            <el-tag :type="row.status === 'active' ? 'success' : 'info'" size="small">
              {{ row.status === 'active' ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="fail_count" label="失败次数" width="100" align="center" />
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
            <el-button
              link
              type="primary"
              size="small"
              :loading="testingId === row.id"
              @click="handleTest(row)"
            >
              测试
            </el-button>
            <el-button link type="danger" size="small" @click="handleDelete(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑站点源' : '新增站点源'"
      width="560px"
      :close-on-click-modal="false"
    >
      <el-form label-width="90px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="站点源名称" maxlength="100" />
        </el-form-item>
        <el-form-item label="站点地址" required>
          <el-input v-model="form.base_url" placeholder="站点地址，如 https://example.com" />
        </el-form-item>
        <el-form-item label="RSS 地址">
          <el-input v-model="form.rss_url" placeholder="RSS 地址（可选）" />
        </el-form-item>
        <el-form-item label="角色">
          <el-input v-model="form.role" placeholder="如 source（默认）" />
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
        <el-form-item label="Passkey">
          <el-input
            v-model="form.passkey"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="masked.passkey ? '已保存，留空表示不修改' : '请输入 Passkey（可选）'"
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
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
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
</style>
