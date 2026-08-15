<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import api from '../api'
import type { DispatchMode, JSONObject, Strategy } from '../api/types'

interface StrategyForm {
  promotions: string[]
  keywords: string[]
  min_size: number
  max_size: number
  retire_seeders: number
  retire_minutes: number
  retire_ratio_enabled: boolean
  retire_ratio: number
  retire_mode: 'and' | 'or'
  dispatch_mode: DispatchMode
  timezone: string
  image_host_text: string
  image_cover_enabled: boolean
  retry_max: number
}

const form = reactive<StrategyForm>({
  promotions: [],
  keywords: [],
  min_size: 0,
  max_size: 0,
  retire_seeders: 0,
  retire_minutes: 0,
  retire_ratio_enabled: false,
  retire_ratio: 1,
  retire_mode: 'and',
  dispatch_mode: 'priority',
  timezone: '',
  image_host_text: '',
  image_cover_enabled: false,
  retry_max: 3,
})

const dispatchOptions: { label: string; value: DispatchMode }[] = [
  { label: '优先（按优先级）', value: 'priority' },
  { label: '手动', value: 'manual' },
  { label: '剩余空间最多', value: 'most_free_disk' },
  { label: '任务最少', value: 'least_jobs' },
  { label: '轮询', value: 'round_robin' },
]

const keywordInput = ref('')
const promotionInput = ref('')
const loading = ref(false)
const saving = ref(false)

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

function applyStrategy(data: Strategy | null | undefined): void {
  form.promotions = Array.isArray(data?.promotions) ? [...data.promotions] : []
  form.keywords = Array.isArray(data?.keywords) ? [...data.keywords] : []
  form.min_size = data?.min_size ?? 0
  form.max_size = data?.max_size ?? 0
  form.retire_seeders = data?.retire_seeders ?? 0
  form.retire_minutes = data?.retire_minutes ?? 0
  form.retire_ratio_enabled = data?.retire_ratio_enabled ?? false
  form.retire_ratio = data?.retire_ratio ?? 1
  form.retire_mode = data?.retire_mode === 'or' ? 'or' : 'and'
  form.dispatch_mode = (data?.dispatch_mode as DispatchMode) ?? 'priority'
  form.timezone = data?.timezone ?? ''
  form.image_host_text = data?.image_host ? JSON.stringify(data.image_host, null, 2) : ''
  form.image_cover_enabled = data?.image_cover_enabled ?? false
  form.retry_max = data?.retry_max ?? 3
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const { data } = await api.get<Strategy>('/strategy')
    applyStrategy(data)
  } catch {
    // 错误统一由响应拦截器提示
  } finally {
    loading.value = false
  }
}

function addKeyword(): void {
  const kw = keywordInput.value.trim()
  if (!kw) return
  if (!form.keywords.includes(kw)) {
    form.keywords.push(kw)
  }
  keywordInput.value = ''
}

function removeKeyword(kw: string): void {
  const idx = form.keywords.indexOf(kw)
  if (idx >= 0) form.keywords.splice(idx, 1)
}

function addPromotion(): void {
  const p = promotionInput.value.trim()
  if (!p) return
  if (!form.promotions.includes(p)) {
    form.promotions.push(p)
  }
  promotionInput.value = ''
}

function removePromotion(p: string): void {
  const idx = form.promotions.indexOf(p)
  if (idx >= 0) form.promotions.splice(idx, 1)
}

async function save(): Promise<void> {
  if (saving.value) return
  const imageHost = parseJSON(form.image_host_text)
  if (!imageHost.ok) {
    ElMessage.error('图片托管配置必须是 JSON 对象')
    return
  }
  saving.value = true
  try {
    const payload: Strategy = {
      id: 1,
      promotions: form.promotions,
      keywords: form.keywords,
      min_size: form.min_size,
      max_size: form.max_size,
      retire_seeders: form.retire_seeders,
      retire_minutes: form.retire_minutes,
      retire_ratio_enabled: form.retire_ratio_enabled,
      retire_ratio: form.retire_ratio,
      retire_mode: form.retire_mode,
      dispatch_mode: form.dispatch_mode,
      timezone: form.timezone,
      image_host: imageHost.value ?? null,
      image_cover_enabled: form.image_cover_enabled,
      retry_max: form.retry_max,
    }
    await api.put('/strategy', payload)
    ElMessage.success('策略已保存')
  } catch {
    // 错误统一由响应拦截器提示
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="strategy" v-loading="loading">
    <el-card shadow="never">
      <template #header>
        <div class="card-header">
          <span class="card-title">抓取 / 分派策略</span>
        </div>
      </template>

      <el-form label-width="160px">
        <el-form-item label="关键词">
          <div class="tag-editor">
            <div class="tag-list">
              <el-tag
                v-for="kw in form.keywords"
                :key="kw"
                closable
                class="tag"
                @close="removeKeyword(kw)"
              >
                {{ kw }}
              </el-tag>
              <span v-if="form.keywords.length === 0" class="muted">空 = 全部通过</span>
            </div>
            <div class="tag-input-row">
              <el-input
                v-model="keywordInput"
                placeholder="输入关键词后回车"
                class="tag-input"
                @keyup.enter="addKeyword"
              />
              <el-button @click="addKeyword">添加</el-button>
            </div>
          </div>
        </el-form-item>

        <el-form-item label="促销（promotions）">
          <div class="tag-editor">
            <div class="tag-list">
              <el-tag
                v-for="p in form.promotions"
                :key="p"
                closable
                class="tag"
                @close="removePromotion(p)"
              >
                {{ p }}
              </el-tag>
              <span v-if="form.promotions.length === 0" class="muted">空 = 全部通过</span>
            </div>
            <div class="tag-input-row">
              <el-input
                v-model="promotionInput"
                placeholder="如 free / 2x，输入后回车"
                class="tag-input"
                @keyup.enter="addPromotion"
              />
              <el-button @click="addPromotion">添加</el-button>
            </div>
          </div>
        </el-form-item>

        <el-form-item label="体积下限">
          <div class="inline-field">
            <el-input-number v-model="form.min_size" :min="0" controls-position="right" />
            <span class="unit-hint">0 表示不限</span>
          </div>
        </el-form-item>

        <el-form-item label="体积上限">
          <div class="inline-field">
            <el-input-number v-model="form.max_size" :min="0" controls-position="right" />
            <span class="unit-hint">0 表示不限</span>
          </div>
        </el-form-item>

        <el-divider content-position="left">自动撤种</el-divider>

        <el-form-item label="做种数阈值">
          <el-input-number v-model="form.retire_seeders" :min="0" controls-position="right" />
        </el-form-item>

        <el-form-item label="做种时长（分钟）">
          <el-input-number v-model="form.retire_minutes" :min="0" controls-position="right" />
        </el-form-item>

        <el-form-item label="启用分享率撤种">
          <el-switch v-model="form.retire_ratio_enabled" />
        </el-form-item>

        <el-form-item label="分享率阈值">
          <el-input-number v-model="form.retire_ratio" :min="0" :precision="2" controls-position="right" />
        </el-form-item>

        <el-form-item label="撤种条件">
          <el-radio-group v-model="form.retire_mode">
            <el-radio value="and">同时满足（且）</el-radio>
            <el-radio value="or">任一满足（或）</el-radio>
          </el-radio-group>
        </el-form-item>

        <el-divider content-position="left">分派</el-divider>

        <el-form-item label="分派模式">
          <el-select v-model="form.dispatch_mode" style="width: 260px">
            <el-option
              v-for="opt in dispatchOptions"
              :key="opt.value"
              :label="opt.label"
              :value="opt.value"
            />
          </el-select>
        </el-form-item>

        <el-form-item label="时区">
          <el-input v-model="form.timezone" placeholder="如 Asia/Shanghai" style="width: 260px" />
        </el-form-item>

        <el-form-item label="图片托管配置">
          <el-input
            v-model="form.image_host_text"
            type="textarea"
            :rows="4"
            placeholder='JSON 对象（可选），例如：{"host":"img.example.com"}'
          />
        </el-form-item>

        <el-form-item label="启用封面图">
          <el-switch v-model="form.image_cover_enabled" />
        </el-form-item>

        <el-form-item label="重试上限">
          <el-input-number v-model="form.retry_max" :min="0" controls-position="right" />
        </el-form-item>

        <el-form-item>
          <el-button type="primary" :loading="saving" @click="save">保存策略</el-button>
        </el-form-item>
      </el-form>
    </el-card>
  </div>
</template>

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

.tag-editor {
  width: 100%;
}

.tag-list {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  min-height: 32px;
  margin-bottom: 12px;
}

.tag {
  height: 28px;
  line-height: 26px;
}

.tag-input-row {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
}

.tag-input {
  max-width: 360px;
}

.inline-field {
  display: flex;
  align-items: center;
  gap: 8px;
}

.unit-hint {
  font-size: 13px;
  color: var(--el-text-color-regular);
}

.muted {
  color: var(--el-text-color-secondary);
  font-size: 13px;
}
</style>
