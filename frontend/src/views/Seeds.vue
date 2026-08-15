<template>
  <div class="seeds">
    <!-- 工具栏：状态筛选 + 刷新 -->
    <div class="toolbar">
      <div class="toolbar-left">
        <span class="toolbar-label">状态筛选</span>
        <el-select v-model="status" placeholder="全部状态" class="status-select" @change="onStatusChange">
          <el-option v-for="opt in statusOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
        </el-select>
      </div>
      <el-button :loading="loading" @click="loadList">刷新</el-button>
    </div>

    <!-- 列表 -->
    <el-card shadow="never">
      <el-table v-loading="loading" :data="list" stripe>
        <el-table-column prop="title" label="标题" min-width="220" show-overflow-tooltip />
        <el-table-column prop="source_site" label="来源" width="140" show-overflow-tooltip />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="statusTagType(row.status)" size="small">{{ statusLabel(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="大小" width="110">
          <template #default="{ row }">{{ bytesToHuman(row.size) }}</template>
        </el-table-column>
        <el-table-column label="创建时间" width="180">
          <template #default="{ row }">{{ formatUnix(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="180" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="openDetail(row)">详情</el-button>
            <el-button link type="warning" size="small" @click="openResend(row)">重发</el-button>
            <el-button link type="danger" size="small" @click="onDelete(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无种子数据" :image-size="80" />
        </template>
      </el-table>

      <div class="pagination">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="onPageChange"
        />
      </div>
    </el-card>

    <!-- 详情抽屉 -->
    <el-drawer v-model="drawerVisible" title="种子详情" size="640px">
      <div v-loading="detailLoading" class="drawer-body">
        <template v-if="detail">
          <el-descriptions :column="2" border>
            <el-descriptions-item label="标题" :span="2">{{ detail.seed.title }}</el-descriptions-item>
            <el-descriptions-item label="来源">{{ detail.seed.source_site || '—' }}</el-descriptions-item>
            <el-descriptions-item label="状态">
              <el-tag :type="statusTagType(detail.seed.status)" size="small">
                {{ statusLabel(detail.seed.status) }}
              </el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="大小">{{ bytesToHuman(detail.seed.size) }}</el-descriptions-item>
            <el-descriptions-item label="重试次数">{{ detail.seed.retry_count }}</el-descriptions-item>
            <el-descriptions-item label="创建时间">{{ formatUnix(detail.seed.created_at) }}</el-descriptions-item>
            <el-descriptions-item label="错误信息" :span="2">{{ detail.seed.error || '—' }}</el-descriptions-item>
          </el-descriptions>

          <div class="section-title">发布 / 辅种记录</div>
          <el-table :data="detail.records" size="small" empty-text="暂无记录">
            <el-table-column prop="target_id" label="目标" width="80" />
            <el-table-column prop="status" label="状态" width="120" />
            <el-table-column prop="attempts" label="尝试" width="70" align="center" />
            <el-table-column prop="last_error" label="最近错误" min-width="180" show-overflow-tooltip />
          </el-table>

          <div class="section-title">副本</div>
          <el-table :data="detail.replicas" size="small" empty-text="暂无副本">
            <el-table-column prop="qb_id" label="qB" width="80" />
            <el-table-column prop="role" label="角色" width="90" />
            <el-table-column prop="status" label="状态" width="110" />
            <el-table-column label="进度" width="140">
              <template #default="{ row }">
                <el-progress :percentage="Math.round(row.progress * 100)" />
              </template>
            </el-table-column>
          </el-table>

          <div class="section-title">日志</div>
          <el-table :data="detail.logs" size="small" empty-text="暂无日志">
            <el-table-column label="级别" width="80">
              <template #default="{ row }">
                <el-tag :type="levelTagType(row.level)" size="small">{{ row.level }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="action" label="动作" width="140" />
            <el-table-column prop="detail" label="详情" min-width="200" show-overflow-tooltip />
          </el-table>
        </template>
      </div>
    </el-drawer>

    <!-- 重发对话框 -->
    <el-dialog v-model="resendVisible" title="重发种子" width="420px">
      <p class="dialog-tip">选择重发方式：</p>
      <div class="resend-options">
        <el-radio-group v-model="resendFull">
          <el-radio :value="false">仅重试失败目标</el-radio>
          <el-radio :value="true">完整重跑（全部目标）</el-radio>
        </el-radio-group>
      </div>
      <template #footer>
        <el-button @click="resendVisible = false">取消</el-button>
        <el-button type="primary" :loading="resending" @click="onResend">确认重发</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'
import type { Seed, SeedDetail } from '../api/types'

const list = ref<Seed[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const status = ref('')
const loading = ref(false)

const statusOptions: Array<{ label: string; value: string }> = [
  { label: '全部状态', value: '' },
  { label: '发现', value: 'discovered' },
  { label: '下载中', value: 'downloading' },
  { label: '已下载', value: 'downloaded' },
  { label: '处理中', value: 'processing' },
  { label: '辅种中', value: 'seeding' },
  { label: '待重试', value: 'retry' },
  { label: '失败', value: 'failed' },
  { label: '已撤种', value: 'retired' },
  { label: '跳过', value: 'skipped' },
]

const statusLabelMap: Record<string, string> = {
  discovered: '发现',
  downloading: '下载中',
  downloaded: '已下载',
  processing: '处理中',
  seeding: '辅种中',
  retry: '待重试',
  failed: '失败',
  retired: '已撤种',
  skipped: '跳过',
}

function statusLabel(s: string): string {
  return statusLabelMap[s] ?? s
}

function statusTagType(s: string): 'success' | 'info' | 'warning' | 'danger' | 'primary' {
  switch (s) {
    case 'seeding':
    case 'downloaded':
      return 'success'
    case 'downloading':
    case 'processing':
      return 'primary'
    case 'retry':
      return 'warning'
    case 'failed':
      return 'danger'
    default:
      return 'info'
  }
}

function levelTagType(level: string): 'success' | 'info' | 'warning' | 'danger' {
  switch (level) {
    case 'success':
      return 'success'
    case 'warning':
      return 'warning'
    case 'error':
      return 'danger'
    default:
      return 'info'
  }
}

function bytesToHuman(bytes?: number): string {
  if (bytes == null || bytes <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let n = bytes
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(n >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

function formatUnix(sec?: number): string {
  if (sec == null || sec <= 0) return '—'
  const d = new Date(sec * 1000)
  if (Number.isNaN(d.getTime())) return String(sec)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

async function loadList(): Promise<void> {
  loading.value = true
  try {
    const { data } = await api.get<{ items: Seed[]; total: number; page: number; size: number }>(
      '/seeds',
      { params: { status: status.value || undefined, page: page.value, size: size.value } },
    )
    list.value = data.items ?? []
    total.value = data.total ?? 0
    page.value = data.page ?? page.value
    size.value = data.size ?? size.value
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    loading.value = false
  }
}

function onStatusChange(): void {
  page.value = 1
  loadList()
}

function onPageChange(p: number): void {
  page.value = p
  loadList()
}

// ============ 详情 ============
const drawerVisible = ref(false)
const detailLoading = ref(false)
const detail = ref<SeedDetail | null>(null)

async function openDetail(row: Seed): Promise<void> {
  drawerVisible.value = true
  detailLoading.value = true
  detail.value = null
  try {
    const { data } = await api.get<SeedDetail>(`/seeds/${row.id}`)
    detail.value = data
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    detailLoading.value = false
  }
}

// ============ 重发 ============
const resendVisible = ref(false)
const resendFull = ref(false)
const resending = ref(false)
const resendId = ref<number | null>(null)

function openResend(row: Seed): void {
  resendId.value = row.id
  resendFull.value = false
  resendVisible.value = true
}

async function onResend(): Promise<void> {
  if (resendId.value == null) return
  resending.value = true
  try {
    await api.post(`/seeds/${resendId.value}/resend`, { full: resendFull.value })
    ElMessage.success('已提交重发')
    resendVisible.value = false
    await loadList()
  } catch {
    // 错误由响应拦截器统一提示
  } finally {
    resending.value = false
  }
}

// ============ 删除 ============
async function onDelete(row: Seed): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除种子「${row.title}」？删除后不可恢复。`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.delete(`/seeds/${row.id}`)
    ElMessage.success('已删除')
    await loadList()
  } catch {
    // 错误由响应拦截器统一提示
  }
}

onMounted(loadList)
</script>

<style scoped>
.seeds {
  padding: 16px;
}

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}

.toolbar-left {
  display: flex;
  align-items: center;
  gap: 12px;
}

.toolbar-label {
  color: #606266;
  font-size: 14px;
}

.status-select {
  width: 160px;
}

.pagination {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}

.drawer-body {
  min-height: 200px;
}

.section-title {
  margin: 20px 0 12px;
  font-size: 15px;
  font-weight: 600;
  color: #303133;
}

.dialog-tip {
  margin: 0 0 12px;
  color: #606266;
  font-size: 14px;
}

.resend-options {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
</style>
