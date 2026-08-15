// AutoSeedRelay 前端类型定义
// 与后端 v2 API 契约对齐（后端存储模型 + wire DTO 为权威）。
// 约定：实体 id 为 number；时间戳为 Unix 秒（number）；凭据字段读回时后端返回 '***' 掩码。

// ============ 通用 ============

/** 业务错误体：后端统一返回 { error: string } */
export interface ApiErrorBody {
  error: string
}

/** 通用 OK 响应 */
export interface OkResponse {
  ok: boolean
}

/** 通用分页结果（GET /seeds 等） */
export interface PageResult<T> {
  items: T[]
  total: number
  page: number
  size: number
}

/** 连接 / 探测 / 测试通用结果 */
export interface TestResult {
  ok: boolean
  message?: string
  latency_ms?: number
  version?: string
}

/** 通用 JSON 对象（维度覆盖 / 覆盖映射 / 通知配置等） */
export type JSONObject = Record<string, unknown>

// ============ 认证 / 初始化 ============

export interface LoginRequest {
  password: string
}

export interface SetupStatus {
  initialized: boolean
}

export interface SetupCompleteRequest {
  password: string
}

// ============ 站点源 Sources ============

export interface Source {
  id: number
  name: string
  role: string
  base_url: string
  rss_url: string
  /** 凭据（passkey / api_token / cookie），已保存时后端返回 '***' 掩码 */
  passkey: string
  api_token: string
  cookie: string
  /** active | paused */
  status: string
  fail_count: number
}

export interface SourceInput {
  name: string
  role?: string
  base_url: string
  rss_url?: string
  passkey?: string
  api_token?: string
  cookie?: string
  status?: string
}

// ============ 目标站 Targets ============

export interface Target {
  id: number
  name: string
  /** nexusphp | nexusphp_classic | mteam */
  type: string
  base_url: string
  announce: string
  passkey: string
  cookie: string
  api_token: string
  test_mode: boolean
  category_overrides: JSONObject | null
  dimension_overrides: JSONObject | null
  tags_map: JSONObject | null
  fallback_category: number | null
  /** active | paused */
  status: string
}

export interface TargetInput {
  name: string
  type?: string
  base_url?: string
  announce?: string
  passkey?: string
  cookie?: string
  api_token?: string
  test_mode?: boolean
  category_overrides?: JSONObject | null
  dimension_overrides?: JSONObject | null
  tags_map?: JSONObject | null
  fallback_category?: number | null
  status?: string
}

// ============ qBittorrent 实例 ============

export interface QBInstance {
  id: number
  name: string
  host: string
  port: number
  username: string
  /** 密码，已保存时后端返回 '***' 掩码 */
  password: string
  priority: number
  enabled: boolean
  extra: JSONObject | null
}

export interface QBInput {
  name: string
  host: string
  port?: number
  username: string
  password?: string
  priority?: number
  enabled?: boolean
  extra?: JSONObject | null
}

// ============ 策略 Strategy ============

/** 分派模式（dispatcher 实际支持的值） */
export type DispatchMode = 'priority' | 'manual' | 'most_free_disk' | 'least_jobs' | 'round_robin'

export interface Strategy {
  id: number
  promotions: string[]
  keywords: string[]
  min_size: number
  max_size: number
  retire_seeders: number
  retire_minutes: number
  retire_ratio_enabled: boolean
  retire_ratio: number
  /** and | or */
  retire_mode: 'and' | 'or'
  dispatch_mode: string
  timezone: string
  image_host: JSONObject | null
  image_cover_enabled: boolean
  retry_max: number
  disk_low_gb: number
  disk_critical_gb: number
  low_speed_kbps: number
  low_speed_duration_sec: number
  /** abort（中止下载） | warn（仅告警） */
  low_speed_action: 'abort' | 'warn'
}

export type StrategyInput = Strategy

// ============ 通知 Notifiers ============

export interface Notifier {
  id: number
  name: string
  /** webhook | telegram | smtp | ntfy | gotify | serverchan | pushplus */
  type: string
  /** 通知器配置（原生 JSON 对象） */
  config: Record<string, unknown>
  enabled: boolean
}

export interface NotifierInput {
  name: string
  type: string
  config: Record<string, unknown>
  enabled: boolean
}

// ============ 通知路由矩阵 ============

/** 路由矩阵单元：某通知实例 × 某级别 的开关（后端 notifier_routes 行） */
export interface NotifierRoute {
  instance_id: number
  /** critical | warning | info */
  tier: 'critical' | 'warning' | 'info'
}

// ============ 事件 ============

export type EventLevel = 'info' | 'success' | 'warning' | 'error'

/** activity_log 行（后端 logJSON） */
export interface EventItem {
  id: number
  seed_id: number
  level: EventLevel
  action: string
  detail: string
  /** Unix 秒 */
  created_at: number
}

// ============ 种子 Seeds ============

/** seeds.status 允许值（见后端 store/status.go） */
export type SeedStatus =
  | 'discovered'
  | 'downloading'
  | 'downloaded'
  | 'processing'
  | 'seeding'
  | 'retry'
  | 'failed'
  | 'retired'
  | 'skipped'

export interface Seed {
  id: number
  source_site: string
  title: string
  info_hash: string
  status: string
  promotion: string
  size: number
  retry_count: number
  /** Unix 秒（后端 discovered_at 暴露为 created_at） */
  created_at: number
  updated_at: number
  error: string
}

/** 单条发布 / 辅种记录（后端 recordJSON） */
export interface SeedRecord {
  id: number
  seed_id: number
  target_id: number
  role: string
  status: string
  target_torrent_id: string
  attempts: number
  last_error: string
  published_at: number
  retired_at: number
  retire_reason: string
  created_at: number
  updated_at: number
}

/** 种子副本（后端 replicaJSON） */
export interface SeedReplica {
  id: number
  seed_id: number
  qb_id: number
  info_hash: string
  role: string
  status: string
  progress: number
  added_at: number
}

/** 种子日志（后端 logJSON） */
export interface SeedLog {
  id: number
  seed_id: number
  level: EventLevel
  action: string
  detail: string
  created_at: number
}

/** GET /seeds/{id} 返回的包裹对象 */
export interface SeedDetail {
  seed: Seed
  records: SeedRecord[]
  replicas: SeedReplica[]
  logs: SeedLog[]
}

export interface SeedListQuery {
  status?: string
  page?: number
  size?: number
}

export interface ResendRequest {
  full: boolean
}

// ============ Dashboard ============

export interface QbStatusItem {
  name: string
  online: boolean
  version: string
}

export interface SourceStatusItem {
  id: number
  name: string
  status: string
  fail_count: number
}

export interface DashboardStatus {
  qbs: QbStatusItem[]
  qb_online: number
  qb_total: number
  sources: SourceStatusItem[]
  disk: { free_gb: number; total_gb: number }
  uptime_seconds: number
  engine: { running: boolean; workers: number }
}

/** 后端 dashboard.stats 的 7 个字段 */
export interface DashboardStats {
  total_published: number
  total_cross_seeded: number
  current_seeding: number
  retry: number
  failed: number
  today_published: number
  today_cross_seeded: number
}

export interface TrendPoint {
  date: string
  count: number
}

export interface DashboardData {
  status: DashboardStatus
  stats: DashboardStats
  /** 最近种子（后端 seedJSON 数组） */
  tasks: Seed[]
  events: EventItem[]
  trend: TrendPoint[]
}

// ============ 备份 ============

export interface RestoreResult {
  ok: boolean
  message?: string
  restart_required?: boolean
}
