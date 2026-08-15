<template>
  <div class="dashboard">
    <!-- 1. 顶部状态条 -->
    <header class="topbar">
      <div class="brand">AutoSeedRelay</div>
      <div class="status">
        <el-tag type="success">引擎运行中</el-tag>
        <el-tag type="info">qB 在线 {{ qbOnline }}</el-tag>
        <el-button size="small" @click="onLogout">退出</el-button>
      </div>
    </header>

    <main class="content">
      <!-- 2. 六张统计卡 -->
      <el-row :gutter="16">
        <el-col
          v-for="card in statCards"
          :key="card.label"
          :xs="12"
          :sm="8"
          :md="4"
        >
          <el-card class="stat-card" shadow="hover">
            <div class="stat-value">{{ card.value }}</div>
            <div class="stat-label">{{ card.label }}</div>
          </el-card>
        </el-col>
      </el-row>

      <el-row :gutter="16" class="row">
        <!-- 3. 进行中任务区 -->
        <el-col :xs="24" :md="12">
          <el-card header="进行中任务">
            <el-table :data="activeTasks" size="small">
              <el-table-column prop="name" label="任务" />
              <el-table-column prop="stage" label="阶段" width="100" />
              <el-table-column prop="progress" label="进度" width="150">
                <template #default="{ row }">
                  <el-progress :percentage="row.progress" />
                </template>
              </el-table-column>
            </el-table>
          </el-card>
        </el-col>

        <!-- 4. 事件流区 -->
        <el-col :xs="24" :md="12">
          <el-card header="事件流">
            <el-timeline>
              <el-timeline-item
                v-for="e in events"
                :key="e.time + e.text"
                :timestamp="e.time"
                :type="e.type"
              >
                {{ e.text }}
              </el-timeline-item>
            </el-timeline>
          </el-card>
        </el-col>
      </el-row>

      <!-- 5. 7 天趋势区 -->
      <el-card header="7 天趋势" class="row">
        <div class="trend">
          <div v-for="d in trend" :key="d.day" class="trend-col">
            <div class="trend-bar" :style="{ height: d.value * 1.6 + 'px' }" />
            <span class="trend-day">{{ d.day }}</span>
          </div>
        </div>
      </el-card>
    </main>
  </div>
</template>

<script setup lang="ts">
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'

interface StatCard {
  label: string
  value: string | number
}

interface ActiveTask {
  name: string
  stage: string
  progress: number
}

interface EventItem {
  time: string
  text: string
  type: 'primary' | 'success' | 'warning' | 'danger' | 'info'
}

interface TrendPoint {
  day: string
  value: number
}

const router = useRouter()
const auth = useAuthStore()

const qbOnline = '3/3'

const statCards: StatCard[] = [
  { label: '今日发现', value: 128 },
  { label: '今日发布', value: 36 },
  { label: '今日辅种', value: 92 },
  { label: '进行中任务', value: 12 },
  { label: '失败重试', value: 3 },
  { label: 'qB 在线', value: qbOnline },
]

const activeTasks: ActiveTask[] = [
  { name: '【示例】示例种子 A', stage: '下载中', progress: 60 },
  { name: '【示例】示例种子 B', stage: '校验中', progress: 40 },
]

const events: EventItem[] = [
  { time: '10:00', text: '发布成功：示例种子 A', type: 'success' },
  { time: '09:50', text: '降级为辅种：示例种子 B', type: 'warning' },
  { time: '09:40', text: 'qB 实例断开（示例）', type: 'danger' },
]

const trend: TrendPoint[] = [
  { day: '一', value: 40 },
  { day: '二', value: 65 },
  { day: '三', value: 50 },
  { day: '四', value: 80 },
  { day: '五', value: 70 },
  { day: '六', value: 90 },
  { day: '日', value: 55 },
]

function onLogout() {
  auth.clear()
  router.push('/login')
}
</script>

<style scoped>
.dashboard {
  min-height: 100vh;
  background: #f5f7fa;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 24px;
  background: #fff;
  border-bottom: 1px solid #e4e7ed;
}

.brand {
  font-size: 20px;
  font-weight: 600;
  color: #303133;
}

.status {
  display: flex;
  align-items: center;
  gap: 8px;
}

.content {
  padding: 16px 24px 24px;
}

.stat-card {
  text-align: center;
  margin-bottom: 16px;
}

.stat-value {
  font-size: 28px;
  font-weight: 600;
  color: #409eff;
}

.stat-label {
  margin-top: 4px;
  color: #909399;
  font-size: 13px;
}

.row {
  margin-bottom: 16px;
}

.trend {
  display: flex;
  align-items: flex-end;
  gap: 16px;
  height: 200px;
  padding: 8px 4px;
}

.trend-col {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  height: 100%;
  justify-content: flex-end;
}

.trend-bar {
  width: 60%;
  background: linear-gradient(180deg, #409eff, #79bbff);
  border-radius: 4px 4px 0 0;
}

.trend-day {
  color: #909399;
  font-size: 12px;
}
</style>
