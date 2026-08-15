<template>
  <div class="events">
    <el-card shadow="never" class="filter-card">
      <div class="filter-row">
        <span class="filter-label">事件级别</span>
        <el-select v-model="level" class="filter-select" @change="onFilterChange">
          <el-option v-for="opt in levelOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
        </el-select>
      </div>
    </el-card>

    <el-card shadow="never" class="list-card">
      <div v-if="loading && events.length === 0" v-loading="loading" class="center" />
      <el-empty v-else-if="events.length === 0" description="暂无事件" />
      <template v-else>
        <ul class="event-list">
          <li v-for="e in events" :key="e.id" class="event-item">
            <span class="event-time">{{ formatUnix(e.created_at) }}</span>
            <el-tag :type="levelTag(e.level).type" size="small" class="event-level">
              {{ levelTag(e.level).label }}
            </el-tag>
            <el-tag v-if="e.action" type="info" size="small" effect="plain" class="event-action">
              {{ e.action }}
            </el-tag>
            <span class="event-message">{{ e.detail }}</span>
            <el-tag v-if="e.seed_id" type="info" size="small" effect="plain" class="event-seed">
              种子 #{{ e.seed_id }}
            </el-tag>
          </li>
        </ul>
        <div class="load-more">
          <el-button
            type="primary"
            plain
            :disabled="!hasMore"
            :loading="loading"
            @click="loadMore"
          >
            {{ hasMore ? '加载更多' : '已加载全部' }}
          </el-button>
        </div>
      </template>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import api from '../api'
import type { EventItem, EventLevel } from '../api/types'

interface EventsResponse {
  events: EventItem[]
  latest: number
}

const levelOptions: Array<{ label: string; value: '' | EventLevel }> = [
  { label: '全部', value: '' },
  { label: '信息', value: 'info' },
  { label: '成功', value: 'success' },
  { label: '警告', value: 'warning' },
  { label: '错误', value: 'error' },
]

const levelMeta: Record<EventLevel, { type: 'info' | 'success' | 'warning' | 'danger'; label: string }> = {
  info: { type: 'info', label: '信息' },
  success: { type: 'success', label: '成功' },
  warning: { type: 'warning', label: '警告' },
  error: { type: 'danger', label: '错误' },
}

const events = ref<EventItem[]>([])
const level = ref<'' | EventLevel>('')
const loading = ref(false)
const hasMore = ref(true)
const since = ref(0)

let requestId = 0

function formatUnix(sec?: number): string {
  if (sec == null || sec <= 0) return '—'
  const d = new Date(sec * 1000)
  if (Number.isNaN(d.getTime())) return String(sec)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

async function load(reset = false) {
  if (loading.value && !reset) return
  const reqId = ++requestId
  if (reset) {
    since.value = 0
    events.value = []
    hasMore.value = true
  }
  loading.value = true
  try {
    const params: Record<string, string | number> = {}
    if (since.value) params.since = since.value
    if (level.value) params.level = level.value

    const { data } = await api.get<EventsResponse>('/events', { params })
    if (reqId !== requestId) return

    const list = data.events ?? []
    if (list.length === 0) {
      hasMore.value = false
    } else {
      since.value = data.latest ?? since.value
      if (reset) {
        events.value = list
      } else {
        const seen = new Set(events.value.map((e) => e.id))
        let added = 0
        for (const item of list) {
          if (!seen.has(item.id)) {
            events.value.push(item)
            seen.add(item.id)
            added++
          }
        }
        hasMore.value = added > 0
      }
    }
  } catch {
    // 错误已由响应拦截器统一提示
  } finally {
    if (reqId === requestId) {
      loading.value = false
    }
  }
}

function levelTag(level: EventLevel) {
  return levelMeta[level] ?? levelMeta.info
}

function onFilterChange() {
  load(true)
}

function loadMore() {
  load(false)
}

onMounted(() => {
  load(true)
})
</script>

<style scoped>
.events {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.filter-card :deep(.el-card__body) {
  padding: 16px 20px;
}

.filter-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

.filter-label {
  color: #606266;
  font-size: 14px;
  font-weight: 500;
}

.filter-select {
  width: 180px;
}

.list-card :deep(.el-card__body) {
  padding: 0;
}

.center {
  min-height: 240px;
}

.event-list {
  margin: 0;
  padding: 0;
  list-style: none;
}

.event-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 14px 20px;
  border-bottom: 1px solid #f0f2f5;
}

.event-item:last-child {
  border-bottom: none;
}

.event-time {
  flex-shrink: 0;
  color: #909399;
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.event-level {
  flex-shrink: 0;
}

.event-action {
  flex-shrink: 0;
}

.event-message {
  flex: 1;
  color: #303133;
  font-size: 14px;
  line-height: 1.5;
  word-break: break-all;
}

.event-seed {
  flex-shrink: 0;
}

.load-more {
  display: flex;
  justify-content: center;
  padding: 16px;
}
</style>
