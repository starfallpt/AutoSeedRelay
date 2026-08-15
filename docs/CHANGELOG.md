# Changelog

AutoSeedRelay v4 重构里程碑记录。架构依据见 [ARCHITECTURE-v4.md](ARCHITECTURE-v4.md)，业务语义见 [BIZ-SPEC.md](BIZ-SPEC.md)。

## M0 — 骨架

- 唯一入口 `relay serve`（`backend/cmd/relay`）；Gin 引擎装配（Recovery / RequestID / slog 结构化日志）。
- `/api/v2/health` 健康检查；登录占位（auth 未接线时返回 503）。
- 自研嵌入式迁移框架（`backend/internal/store/migrations/*.sql`，PRAGMA user_version 追踪，逐迁移事务）。
- `webfs` 嵌入前端构建产物；前端脚手架（Vue 3 + Vite + TS + Element Plus + Pinia + axios）+ 登录页。

## M1 — 基础

- `secret`：主密钥生命周期（env `AUTOSEED_MASTER_KEY` / `data/master.key` / 首启生成，0600）+ AES-256-GCM 加解密 + 掩码。
- `config`：部署级配置（YAML + env 覆盖：`listen_addr`/`log_level`/`db_path`）。
- `store`：全表迁移 `00001_init.sql`（sources/targets/qb_instances/seeds/relay_records/seed_replicas/activity_log/notifier_instances/notifier_routes/strategies）。
- `qb`：多实例客户端 + 连接池；`bencode` 加固移植（深度/大小/溢出限制）。

## M2 — 核心

- `source`/`adapters`/`titler`/`descr` 移植+修复（SSRF 加固、响应体限流、三套目标站适配器）。
- `pipeline`：RelayOne v2 下载→详情→清洗→发布/交叉辅种（校验降级）；目标级重试（PartialFailure，仅重跑未完成目标）。
- `engine`：Poller / Monitor / Dispatcher / RetryQueue（退避 60s/300s/900s，上限 retry_max）；多 qB 分派（priority/least_jobs/most_free_disk/round_robin）。
- 手动重发 `POST /seeds/{id}/resend`；备份导出/恢复（`GET /backup/export`、`POST /backup/restore` staging 后重启生效）。

## M3 — 面板 + 安全

- auth 全家桶：bcrypt 口令、无状态 HMAC-SHA256 会话（24h）、CSRF 双提交、登录限流（每 IP 5 次/分钟）、SetupGuard；状态落 `app_settings`（`web_password_hash`/`session_secret`）。
- v2 API 全量端点（`/api/v2`：health / auth / setup / dashboard / seeds / events / sources / targets / qb / strategy / notifiers / backup）。
- 前端 11 个 view：Login + Setup + 9 主页面（Dashboard / Seeds / Events / Sources / Targets / QB / Strategy / Notifiers / Backup）。
- 通知系统：7 个 provider 全实现；实例 × tier × 事件聚合；每实例熔断器；Telegram `editMessageText` 防刷屏。
- engine 磁盘/低速率监控（`strategies` 新增 disk_low_gb/disk_critical_gb/low_speed_kbps/low_speed_duration_sec/low_speed_action）。
- 永久去重 tombstone（`seen_hashes` 表，`00006_seen_hashes.sql`）。

## M4 / M5 — 进行中

- M4：三个生产目标站真实发布+辅种验证（用户确认 + 缺陷修复）。
- M5：删旧代码、CI（测试矩阵/race/gosec/trivy/镜像签名）、文档同步与部署配置核对。
