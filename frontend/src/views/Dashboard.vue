<template>
  <div class="dashboard" v-loading="loading">
    <!-- 1. 顶部状态条 -->
    <div class="status-bar">
      <div class="status-item">
        <span class="status-label">qB 在线</span>
        <el-tag :type="qbTagType" effect="dark">{{ status.qb_online }}/{{ status.qb_total }}</el-tag>
      </div>
      <div class="status-item">
        <span class="status-label">源站</span>
        <span class="status-value">{{ sourceCount }}</span>
      </div>
      <div class="status-item status-disk">
        <span class="status-label">磁盘</span>
        <el-progress
          class="disk-progress"
          :percentage="diskUsedPercent"
          :color="diskColor"
          :stroke-width="10"
        />
      </div>
      <div class="status-item">
        <span class="status-label">引擎</span>
        <el-tag :type="status.engine.running ? 'success' : 'danger'" effect="dark">
          {{ status.engine.running ? '运行中' : '已停止' }}
        </el-tag>
      </div>
    </div>

    <!-- 2. 统计卡 -->
    <el-row :gutter="16" class="stat-row">
      <el-col v-for="card in statCards" :key="card.label" :xs="12" :sm="8" :md="4">
        <el-card class="stat-card" shadow="hover">
          <div class="stat-value">{{ card.value }}</div>
          <div class="stat-label">{{ card.label }}</div>
        </el-card>
      </el-col>
    </el-row>

    <!-- 3 + 4. 最近种子 / 事件流 -->
    <el-row :gutter="16" class="row">
      <el-col :xs="24" :lg="12">
        <el-card header="最近种子" class="panel-card">
          <el-table :data="tasks" row-key="id" empty-text="暂无种子" size="small">
            <el-table-column prop="title" label="标题" show-overflow-tooltip />
            <el-table-column prop="status" label="状态" width="110" />
            <el-table-column label="大小" width="110">
              <template #default="{ row }">{{ bytesToHuman(row.size) }}</template>
            </el-table-column>
          </el-table>
        </el-card>
      </el-col>

      <el-col :xs="24" :lg="12">
        <el-card header="事件流" class="panel-card">
          <el-timeline v-if="events.length" class="event-timeline">
            <el-timeline-item
              v-for="e in events"
              :key="e.id"
              :timestamp="formatUnix(e.created_at)"
              :type="levelToType(e.level)"
            >
              <span v-if="e.action" class="event-action">[{{ e.action }}]</span> {{ e.detail }}
            </el-timeline-item>
          </el-timeline>
          <el-empty v-else description="暂无事件" :image-size="60" />
        </el-card>
      </el-col>
    </el-row>

    <!-- 5. 7 天趋势 -->
    <el-card header="7 天趋势" class="trend-card">
      <div v-if="trend.length" class="trend">
        <div v-for="d in trend" :key="d.date" class="trend-col">
          <span class="trend-value">{{ d.count }}</span>
          <div class="trend-bar" :style="{ height: barHeightPx(d.count) }" />
          <span class="trend-day">{{ d.date }}</span>
        </div>
      </div>
      <el-empty v-else description="暂无趋势数据" :image-size="60" />
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import api from '../api'
import type { DashboardData, DashboardStats, DashboardStatus, EventLevel, Seed } from '../api/types'

const data = ref<DashboardData | null>(null)
const loading = ref(true)

const status = computed<DashboardStatus>(
  () =>
    data.value?.status ?? {
      qbs: [],
      qb_online: 0,
      qb_total: 0,
      sources: [],
      disk: { free_gb: 0, total_gb: 0 },
      uptime_seconds: 0,
      engine: { running: false, workers: 0 },
    },
)

const stats = computed<DashboardStats>(
  () =>
    data.value?.stats ?? {
      total_published: 0,
      total_cross_seeded: 0,
      current_seeding: 0,
      retry: 0,
      failed: 0,
      today_published: 0,
      today_cross_seeded: 0,
    },
)

const tasks = computed<Seed[]>(() => data.value?.tasks ?? [])
const events = computed(() => data.value?.events ?? [])
const trend = computed(() => data.value?.trend ?? [])

const sourceCount = computed(() => status.value.sources.length)

const diskUsedPercent = computed(() => {
  const { free_gb, total_gb } = status.value.disk
  if (!total_gb || total_gb <= 0) return 0
  const used = total_gb - free_gb
  const pct = (used / total_gb) * 100
  return Math.max(0, Math.min(100, Math.round(pct)))
})

const qbTagType = computed(() => {
  const { qb_online, qb_total } = status.value
  return qb_total > 0 && qb_online === qb_total ? 'success' : 'warning'
})

const diskColor = computed(() => (diskUsedPercent.value > 85 ? '#f56c6c' : '#409eff'))

interface StatCard {
  label: string
  value: string | number
}

const statCards = computed<StatCard[]>(() => [
  { label: '今日发布', value: stats.value.today_published },
  { label: '今日辅种', value: stats.value.today_cross_seeded },
  { label: '累计发布', value: stats.value.total_published },
  { label: '累计辅种', value: stats.value.total_cross_seeded },
  { label: '当前辅种', value: stats.value.current_seeding },
  { label: '待重试', value: stats.value.retry },
  { label: '失败', value: stats.value.failed },
])

function levelToType(level: EventLevel): 'info' | 'success' | 'warning' | 'danger' {
  if (level === 'success') return 'success'
  if (level === 'warning') return 'warning'
  if (level === 'error') return 'danger'
  return 'info'
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

const MAX_BAR_PX = 160
const maxTrendValue = computed(() => Math.max(1, ...trend.value.map((t) => t.count)))

function barHeightPx(value: number): string {
  return `${Math.round((value / maxTrendValue.value) * MAX_BAR_PX)}px`
}

async function load() {
  loading.value = true
  try {
    const { data: res } = await api.get<DashboardData>('/dashboard')
    data.value = res
  } catch {
    // 错误已由拦截器统一提示，此处不重复弹错
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<style scoped>
.dashboard {
  padding: 16px;
}

/* 状态条 */
.status-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px 32px;
  padding: 14px 20px;
  margin-bottom: 16px;
  background: #fff;
  border-radius: 6px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.06);
}

.status-item {
  display: flex;
  align-items: center;
  gap: 8px;
}

.status-label {
  color: #909399;
  font-size: 13px;
}

.status-value {
  font-size: 16px;
  font-weight: 600;
  color: #303133;
}

.status-disk {
  flex: 1;
  min-width: 200px;
}

.disk-progress {
  flex: 1;
  max-width: 220px;
}

/* 统计卡 */
.stat-row {
  margin-bottom: 16px;
}

.stat-card {
  margin-bottom: 16px;
  text-align: center;
}

.stat-value {
  font-size: 30px;
  font-weight: 600;
  color: #409eff;
  line-height: 1.2;
}

.stat-label {
  margin-top: 6px;
  color: #909399;
  font-size: 13px;
}

/* 面板卡 */
.row {
  margin-bottom: 16px;
}

.panel-card {
  margin-bottom: 16px;
}

.event-timeline {
  padding-left: 4px;
  max-height: 360px;
  overflow-y: auto;
}

.event-action {
  color: #909399;
}

/* 趋势图 */
.trend {
  display: flex;
  align-items: flex-end;
  gap: 16px;
  height: 220px;
  padding: 8px 4px 0;
}

.trend-col {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: flex-end;
  height: 100%;
}

.trend-value {
  margin-bottom: 4px;
  font-size: 12px;
  color: #606266;
}

.trend-bar {
  width: 60%;
  min-height: 4px;
  background: linear-gradient(180deg, #409eff, #79bbff);
  border-radius: 4px 4px 0 0;
  transition: height 0.3s ease;
}

.trend-day {
  margin-top: 8px;
  color: #909399;
  font-size: 12px;
}
</style>
