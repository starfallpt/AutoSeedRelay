<script setup lang="ts">
import { ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import api from '../api'

/** 待恢复的上传文件 */
const restoringFile = ref<File | null>(null)
/** 导出 loading */
const exporting = ref(false)
/** 恢复 loading */
const restoring = ref(false)

const BACKUP_FILENAME_PREFIX = 'autoseedrelay-backup'

/** 生成默认下载文件名：autoseedrelay-backup-<时间戳>.zip */
function defaultFileName(now = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  const stamp =
    `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}_` +
    `${pad(now.getHours())}-${pad(now.getMinutes())}-${pad(now.getSeconds())}`
  return `${BACKUP_FILENAME_PREFIX}-${stamp}.zip`
}

/** 从响应头 Content-Disposition 解析文件名，取不到则用默认名 */
function resolveFileName(contentDisposition: string | undefined): string {
  const fallback = defaultFileName()
  if (!contentDisposition) return fallback
  const utf8 = /filename\*\s*=\s*UTF-8''([^;]+)/i.exec(contentDisposition)
  if (utf8 && utf8[1]) return decodeURIComponent(utf8[1].trim())
  const plain = /filename\s*=\s*"?([^";]+)"?/i.exec(contentDisposition)
  if (plain && plain[1]) return plain[1].trim()
  return fallback
}

/** 触发浏览器下载一个 Blob */
function triggerDownload(blob: Blob, fileName: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

/** 导出全量备份 */
async function handleExport(): Promise<void> {
  if (exporting.value) return
  exporting.value = true
  try {
    const res = await api.get('/backup/export', { responseType: 'blob' })
    const fileName = resolveFileName(res.headers['content-disposition'])
    triggerDownload(res.data as Blob, fileName)
    ElMessage.success('备份已导出')
  } catch {
    // 错误统一由响应拦截器提示，这里不再重复弹出
  } finally {
    exporting.value = false
  }
}

/** el-upload 选择文件时记录待上传文件 */
function handleFileChange(file: { raw?: File }): void {
  restoringFile.value = file.raw ?? null
}

/** 清空已选文件（el-upload 移除文件时回调） */
function handleFileRemove(): void {
  restoringFile.value = null
}

/** 恢复全量备份 */
async function handleRestore(): Promise<void> {
  if (!restoringFile.value || restoring.value) return
  restoring.value = true
  try {
    const formData = new FormData()
    formData.append('file', restoringFile.value)
    await api.post('/backup/restore', formData)
    ElMessage.success('恢复成功')
    ElMessageBox.alert('备份已恢复，需重启服务后生效。', '恢复完成', {
      confirmButtonText: '知道了',
      type: 'success',
    }).catch(() => {
      // 用户点击遮罩或取消时不做任何事
    })
  } catch {
    // 错误统一由响应拦截器提示，这里不再重复弹出
  } finally {
    restoring.value = false
  }
}
</script>

<template>
  <section class="backup-page">
    <el-card shadow="never" class="backup-card">
      <template #header>
        <span class="card-title">备份与恢复</span>
      </template>

      <el-alert
        type="info"
        :closable="false"
        show-icon
        title="说明"
        description="备份与恢复均为全量备份，包含站点源、目标站、qBittorrent 实例、策略、通知器等全部配置。恢复完成后需重启服务方可生效。"
      />

      <div class="backup-section">
        <h3 class="section-title">导出备份</h3>
        <p class="section-desc">将当前全部配置导出为一个 JSON 文件，可在其它环境或后续恢复使用。</p>
        <el-button type="primary" :loading="exporting" @click="handleExport">
          导出备份
        </el-button>
      </div>

      <el-divider />

      <div class="backup-section">
        <h3 class="section-title">恢复备份</h3>
        <p class="section-desc">选择此前导出的备份文件并恢复。恢复为全量覆盖，恢复后需重启服务生效。</p>
        <div class="restore-row">
          <el-upload
            :auto-upload="false"
            :limit="1"
            accept=".zip,application/zip"
            :on-change="handleFileChange"
            :on-remove="handleFileRemove"
            :on-exceed="handleFileRemove"
          >
            <el-button :disabled="restoring">选择备份文件</el-button>
          </el-upload>
          <el-button
            type="primary"
            :disabled="!restoringFile"
            :loading="restoring"
            @click="handleRestore"
          >
            恢复
          </el-button>
        </div>
        <el-text v-if="restoringFile" type="info" size="small">
          已选择：{{ restoringFile.name }}
        </el-text>
      </div>
    </el-card>
  </section>
</template>

<style scoped>
.backup-page {
  max-width: 720px;
}

.backup-card {
  border-radius: 8px;
}

.card-title {
  font-size: 16px;
  font-weight: 600;
}

.backup-section {
  margin-top: 20px;
}

.section-title {
  margin: 0 0 6px;
  font-size: 15px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.section-desc {
  margin: 0 0 16px;
  font-size: 13px;
  line-height: 1.6;
  color: var(--el-text-color-regular);
}

.restore-row {
  display: flex;
  align-items: center;
  gap: 12px;
}
</style>
