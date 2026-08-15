# AutoSeedRelay 重构架构方案 v4

> 状态：M0~M3 已实现，M4（生产验证）/M5（收尾）进行中。本文已按 M3 实现同步（偏差见 §17 实现修订）。
> 配套：docs/BIZ-SPEC.md（业务语义权威）、docs/archive/ARCHITECTURE-v3.md（旧版，仅历史参考）。

## 1. 目标与原则

- **目标**：重写为 Go(Gin) + Vue3 单容器应用，实现 BIZ-SPEC 全部业务语义，修复旧版 P0 安全清单与 23 条业务缺陷。
- **原则**：业务语义以 BIZ-SPEC 为准；旧代码仅作移植参考（archive/）；能复用逻辑的移植+修复，不复用结构；安全修复不进补丁、直接做进新架构。

## 2. 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.22+ · Gin · 手写 SQL 仓储 · 自研嵌入迁移器 · yaml.v3（部署级配置） |
| 存储 | SQLite（modernc 纯 Go）；PostgreSQL 可选/sqlc 为原始预留，M3 未落地（见 §17） |
| 前端 | Vue 3 · Vite · TypeScript · Element Plus · Pinia · axios |
| 打包 | 多阶段 Docker：node 编前端 → Go embed 产物 → alpine 单容器 |
| 测试 | 单测 SQLite / 集成 PG 容器 / race / gosec / trivy |

## 3. 工程结构（monorepo）

```
AutoSeedRelay/
├─ backend/
│  ├─ cmd/relay/main.go        # 唯一入口 serve
│  ├─ internal/
│  │  ├─ config/               # 部署级配置（yaml.v3 + env）：listen_addr/log_level/db_path
│  │  ├─ secret/               # 主密钥 + AES-256-GCM 加解密 + 脱敏
│  │  ├─ store/                # 手写 SQL 仓储 + 嵌入式迁移（migrations/*.sql）
│  │  ├─ qb/                   # qB 客户端 + 多实例连接池
│  │  ├─ source/               # RSS/详情/下载（移植+加固：大小上限/SSRF防护/退避）
│  │  ├─ adapters/             # 目标站适配（nexusphp/classic/mteam）+ 站点枚举探测
│  │  ├─ bencode/              # 移植+加固（溢出/深度/大小限制）
│  │  ├─ titler/ descr/        # 移植+修正（去 Python 怪癖固化）
│  │  ├─ pipeline/             # RelayOne v2：下载→清洗→发布/辅种（校验降级）
│  │  ├─ engine/               # 编排：Poller/Monitor/Dispatcher/RetryQueue
│  │  ├─ notifier/             # 通知：provider→实例→分层路由→聚合
│  │  ├─ backup/               # zip 导出/导入恢复
│  │  ├─ server/               # Gin 装配、路由、依赖注入
│  │  ├─ api/                  # handlers：config(sources/targets/qb/strategy/notifiers) + ops(seeds/events/dashboard/backup)
│  │  └─ webfs/                # embed 前端构建产物
├─ frontend/                   # Vue3 工程（views/components/stores/api）
├─ archive/                    # 旧代码归档（移植参考，不进构建）
├─ deploy/                     # Dockerfile/compose/start（重写，零硬编码凭据）
└─ docs/
```

## 4. 配置分层（决策：业务配置入 DB）

- **部署级（yaml/env，启动只读）**：`listen_addr`、`db.driver`+`db.dsn`、`log_level`、`workdir/torrents_dir`、`session_secret`。
- **业务级（数据库表）**：sources / targets / qb_instances / notifier_instances+notifier_routes / strategies（筛选、撤种、分派、时区、图床开关）。
- 页面改动立即生效（engine 订阅配置变更或轮询重载）；旧 relay.yaml 废弃。

## 5. 凭据安全（决策：加密落盘 + 鉴权后可读）

- 主密钥：env `AUTOSEED_MASTER_KEY`（64 hex）；未设置时首启生成 `data/master.key`（0600）并提示备份。
- 加密字段：站点 cookie/passkey/api_token、目标站凭据、qb_instances.password、notifier_instances.config_json（bot token 等），AES-256-GCM。
- API 返回一律掩码 `***`；面板登录后可「显示/复制」（解密）。
- 备份 zip 默认脱敏；可选「含凭据」导出（提示风险）。

## 6. 数据模型（v2 全新建表）

```
sources(id, name, role, base_url, rss_url, announce_url, status[active|paused],
        fail_count, enc_cookie, enc_passkey, enc_api_token, created_at, updated_at)
targets(id, name, type[nexusphp|nexusphp_classic|mteam], version[api|classic],
        base_url, announce_url, test_mode, fallback_category,
        category_overrides JSON, dimension_overrides JSON, tags_map JSON, enc_* 凭据, status)
qb_instances(id, name, host, port, username, enc_password, priority, enabled,
             last_seen_at, extra JSON)
seeds(id, source_site, info_hash, title, size, category, promotion, source_id,
      status, error, retry_count, discovered_at, updated_at,
      UNIQUE(source_site, info_hash))
relay_records(id, seed_id, target_id, role[publisher|seeder], status,
              target_torrent_id, attempts, last_error, published_at,
              retired_at, retire_reason, UNIQUE(seed_id, target_id))
seed_replicas(id, seed_id, qb_id, info_hash, role[origin|cross], status,
              progress, added_at)                     # 种子×qB 副本
activity_log(id, seed_id, level, action, detail, created_at,
             INDEX(created_at))                        # 滚动保留（默认30天，可配）
notifier_instances(id, type[webhook|telegram|smtp|ntfy|gotify|serverchan|pushplus],
                   name, enc_config, enabled)
notifier_routes(instance_id, tier[critical|warning|info], enabled)
strategies(id=1 单行: promotions/keywords/min_size/max_size,
           retire_seeders/retire_minutes/retire_ratio_enabled/retire_ratio/
           retire_mode[and|or], dispatch_mode, timezone, image_host JSON,
           image_cover_enabled, retry_max,
           disk_low_gb/disk_critical_gb/low_speed_kbps/
           low_speed_duration_sec/low_speed_action)      # M3 磁盘/低速率监控
app_settings(key PRIMARY KEY, value)                     # 应用级 KV（M3：web_password_hash/session_secret）
seen_hashes(source_site, info_hash, first_seen_at,
            PRIMARY KEY(source_site, info_hash))          # 永久去重 tombstone（M5）
```

> 迁移：`backend/internal/store/migrations/00001_init.sql` ~ `00006_seen_hashes.sql`（6 个，自研嵌入迁移器按 PRAGMA user_version 递增应用；00003 仅推进版本号、无 DDL）。

## 7. 引擎设计

- **Poller**：每源站独立 ticker（poll_interval，启动立即一轮）；RSS→筛选→去重→入队。
- **Dispatcher**：多 qB 分派接口 `PickQB(kind)`；策略 priority（手动优先级降序）/ least_jobs / most_free_disk / round_robin；交叉辅种优先同 qB。
- **RetryQueue**：内存延时队列（退避 60s/300s/900s，上限 retry_max=3）；启动时从 status='retry' 的记录重建；失败进 failed + critical 通知（重试耗尽）；部分目标失败（PartialFailure）重试耗尽后保留成功目标、仅告警失败目标。
- **Monitor**：遍历全部启用 qB：连接状态/做种统计/真实磁盘/低速率中止/撤种判定（seeders≥10 或 minutes>60，AND/OR 可配，ratio 默认关）；0 进度副本不计时长。
- **状态机**：种子级 9 态 + 记录级 8 态（BIZ-SPEC §5）；种子终态聚合规则：全部记录终态 + 无副本。
- **优雅退出**：stopCh + wg，所有 goroutine 可追踪。

## 8. 多 qB 与免鉴权拉起

- qB 实例为一等实体（CRUD + 连接测试 + 在线状态/任务数/磁盘展示）。
- **免鉴权拉起**：`/qb/{instance}/` 反向代理（session + CSRF + 仅已初始化 + 上游仅限已配置实例），SID 由后端注入、剥 Set-Cookie；WebSocket Hijack（qB 实时刷新）。
- **Vertex 借鉴（已确认，详见 docs/REF-vertex.md）**：
  - SID 仅存内存、不落盘；启动重登、闲置 >3000s 自动重登；
  - qB API 版本探测 + 缓存 + 兼容分支（login 204/"Ok."、stop/pause、start/resume）；
  - 同步走 `sync/maindata` + 字段白名单（降带宽/内存）；
  - 反代时重写 HTML 相对路径、剥离 X-Forwarded-For/Authorization、补齐 Referer；可配置跳过证书校验（自签场景）。
- 预留 **DownloaderProvider 接口**（借鉴 Vertex 多下载器 registry）：当前实现 qB，未来可扩 Deluge/Transmission。

> 实现修订（M3）：浏览器直连 qB WebUI 的免鉴权反向代理（`/qb/{instance}/` + WebSocket Hijack + SID 注入）**未实现**——当前后端仅以服务端 HTTP 客户端（qb 包）直连 qB 的 `/api/v2/*`，面板不代理 qB WebUI；相应端点已从 §10 移除。

## 9. 通知系统

- Provider 注册表接口 `Send(ctx, msg)`；内置 webhook/telegram/smtp/ntfy/gotify/serverchan/pushplus（M3 全部实现）。
- 实例：同 provider 可多份；notifier_routes 勾选矩阵（实例×tier）。
- 聚合：同实例 × tier × 事件 10 分钟合并（critical 直通不合并）；每日 digest 未实现（预留）。
- **熔断（Vertex 借鉴，实现为每实例熔断器）**：连续失败 5 次熔断 10 分钟，半开探测一次（成功闭合/失败重开）；Telegram 对 10 分钟内的连续消息用 `editMessageText` 复用同一条消息防刷屏。
- 入站 webhook（预留）：apiKey 放路径、独立于 session 鉴权（外部系统反向触发）。
- 事件 tier：critical（重试耗尽/qB 全断/磁盘 critical）；warning（磁盘 low/单 qB 断连/低速率中止/鉴权过期）；info（发布成功/交叉辅种/自动撤种）。
- 全部默认关；测试发送按钮。

## 10. API 契约 v2（base /api/v2，除标注外均需 session + CSRF）

> 本节为 M3 实现后逐条比对的**实际路由**（来源：`backend/internal/server/server.go`、`internal/api/deps.go`、`internal/api/ops_seeds.go`）。相对原始方案删减/更名的端点见 §17。

| 域 | 端点 |
|---|---|
| 健康 | GET /health（无需鉴权） |
| 鉴权 | POST /auth/login · POST /auth/logout · GET /auth/me |
| 向导 | GET /setup/status · POST /setup/complete（**仅未初始化开放**，设置初始面板密码） |
| 仪表盘 | GET /dashboard（status 状态条 + stats 统计卡 + tasks 进行中 + events 事件流 + trend 7天趋势） |
| 种子 | GET /seeds（筛选分页）· GET /seeds/{id}（含 records+replicas+log）· POST /seeds/{id}/resend（手动重发，body 可带 `full=true` 全量重跑）· DELETE /seeds/{id} |
| 事件 | GET /events（活动日志，分页/筛选） |
| 站点源 | GET/POST /sources · GET/PUT/DELETE /sources/{id} · POST /sources/{id}/test |
| 目标站 | GET/POST /targets · GET/PUT/DELETE /targets/{id} · POST /targets/{id}/probe（枚举探测）· POST /targets/{id}/test |
| qB | GET/POST /qb · GET/PUT/DELETE /qb/{id} · POST /qb/{id}/test |
| 策略 | GET/PUT /strategy（单行） |
| 通知 | GET/POST /notifiers · PUT/DELETE /notifiers/{id} · POST /notifiers/{id}/test · GET/PUT /notifiers/routes（路由矩阵） |
| 备份 | GET /backup/export（zip 下载）· POST /backup/restore（multipart zip，校验后 staging，**重启生效**） |

**中间件链（实现修订）**：`Recovery → RequestID → SlogLogger（结构化日志）→ Auth`。Auth 中间件内部：未初始化 403（SetupGuard）→ 未登录 401 → POST/PUT/DELETE 校验 CSRF 403；`/health`、`/auth/login`、`/setup/*` 豁免。

- **会话**：无状态 HMAC-SHA256 签名 cookie（`autoseed_session`，24h TTL，HttpOnly + SameSite=Lax）；签名密钥首启自动生成并持久化于 `app_settings.session_secret`（非内存、非 env）。
- **CSRF**：双提交——`csrf_token` cookie + `X-CSRF-Token` 请求头，常量时间比较。
- **限流**：仅登录接口，每 IP 每分钟 5 次（固定窗口，返回 Retry-After）。
- **可信代理**：`SetTrustedProxies(nil)`——不信任 X-Forwarded-For，客户端 IP 不可伪造。
- **服务器超时**：显式 `http.Server`，Read 15s / Write 60s / Idle 120s / ReadHeader 10s。
- **SSRF 加固**：source 包所有出站请求前 `safeURL` 校验（仅 http/https，解析 IP 拒绝环回/私网/链路本地/组播，重定向逐跳复检）；响应体 `http.MaxBytesReader` 限流（RSS 10MB）。
- **凭据脱敏**：API 返回一律掩码 `***`；错误串/通知正文经 `source.RedactError` 剥离 URL 内嵌凭据（passkey/token）。

## 11. 前端设计

- 页面（11 个 view）：Login / Setup（初始化向导）+ 9 个主页面——Dashboard（五区）/ Seeds（抽屉详情）/ Events（事件流）/ Sources（站点源）/ Targets（目标站）/ QB（多实例）/ Strategy（策略）/ Notifiers（实例+路由矩阵）/ Backup（导出+恢复）。相对原始方案：Config 拆为 Sources/Targets/Strategy 三页，Logs→Events，Preview 页移除（未实现）。
- stores：auth（引导/登录/CSRF）；api/ 下 axios 封装（baseURL `/api/v2`，自动注入 `X-CSRF-Token`，统一处理 401/403）+ TS 类型。
- 图表轻量自绘（趋势柱状），不引重图表库；Element Plus 按需引入。

## 12. Docker 打包（单容器）

```
stage1 node:20-alpine   → npm ci + npm run build 前端 dist/
stage2 golang:1.22-alpine → embed dist/ 编 relay 静态二进制（CGO_ENABLED=0）
stage3 alpine → relay + data/ 卷 + healthcheck（wget HTTP /api/v2/health）
```
- compose：单容器 + 可选外部 qB（`${QB_*}` 变量化，修好 .env 接线）；无任何硬编码凭据。
- 数据：SQLite 文件 + master.key 挂卷；备份导出 zip 含数据库与脱敏配置。

## 13. 迁移计划（5 阶段 + 验收）

| 阶段 | 内容 | 验收 |
|---|---|---|
| M0 骨架 | backend 可起、health、迁移框架、前端脚手架+登录页 | 单容器能跑通登录 |
| M1 基础 | secret/config、store 全表+迁移、qb 多实例客户端、bencode 加固移植、结构化日志 | 单测通过（含 bencode 恶意输入） |
| M2 核心 | source/adapters/titler/descr 移植修复、pipeline+engine 接通、重试队列、多qB分派、手动发种、备份导出/恢复 | dev 测试站全流程无人值守跑通（汇报 dev API 问题） |
| M3 面板 | 全部页面 + 向导安全重做 + 通知系统 + 图床可选 | 面板全功能 + 通知矩阵可用 |
| M4 验证 | 三个生产目标站（代号 T1/T2/T3）各真实发布+辅种各一条，字段人工核对 | 用户确认 + 缺陷修复 |
| M5 收尾 | 删旧代码、CI（sqlite+pg 双引擎/race/gosec/trivy/镜像签名）、文档更新 | P0 安全清单全部关闭 |

**双轨原则**：新工程独立于旧代码开发，旧版持续可用，M4 验证通过后才切换。

## 14. 测试策略

- 单元：bencode（恶意输入 fuzz）/ adapters（三协议 httptest）/ titler / descr / filter / retire / dispatcher / secret / backup / notifier。
- 集成：pipeline 全链路（httptest 假源站+假 qB）；SQLite 与 PG 双引擎 CI 矩阵。
- E2E：Playwright 关键路径冒烟（登录→向导→手动发种→撤种）。

## 15. 风险与依赖

- T3（classic 魔改）适配不确定 → M4 真实验证迭代；DEV API 已知问题 → M2 排查并汇报。
- 站点限流/封禁 → 全链路退避+限速+暂停机制（BIZ-SPEC 已定）。
- Vertex 参考研究 → 已并入 §8/§9（详见 docs/REF-vertex.md）。
- 前端契约漂移 → API 类型从后端生成或集中同步。

## 16. 仓库级重组对照（用户确认：全仓库重构）

| 旧路径 | 处置 |
|---|---|
| cmd/ · internal/（旧 Go 代码） | → backend/（移植+修复）；原始文件归档 archive/ |
| internal/web（模板+原生JS） | → frontend/（Vue3）+ backend/internal/{api,webfs} |
| scripts/*（12 个部署脚本） | → deploy/ 重写（零硬编码凭据）；旧脚本归档 |
| sites/*.yml（站点配置） | 业务配置入 DB 后废弃；作为适配参数迁移参考归档 |
| config/relay.yaml.example | → backend/config.yaml.example（仅部署级）+ 首启向导（业务级） |
| Dockerfile · docker-compose*.yml · start.sh/ps1 | → deploy/ 重写（多阶段单容器、`${QB_*}` 变量化） |
| .github/workflows | 重写：测试矩阵（sqlite+pg+race）+ 多阶段镜像构建 + GHCR 发布（semver 标签）+ trivy 扫描 |
| README.md | 重写：真实功能清单（不虚标）+ 快速开始 + 文档索引 |
| docs/*（15 份旧文档） | BIZ-SPEC/ARCHITECTURE-v4 为权威；旧文档移 docs/archive/ 标注「历史」 |
| LICENSE | 保留 |

**CI 策略（用户确认 GitHub Actions）**：push main → 测试 + 构建 dev 镜像；打 tag → semver 镜像 + latest；初期**不做**自动部署到 VPS，VPS 手动 `docker compose pull && up -d`。

## 17. 实现修订记录（M3 里程碑核对）

本节记录 M3 实现后与原始方案（§1~§16）的偏差，作为版本演进依据；正文相应章节已同步。

| 主题 | 原始方案 | M3 实现 |
|---|---|---|
| 存储/迁移 | sqlc + goose + PG 可选 | 手写 SQL 仓储 + 自研嵌入迁移器（`migrations/*.sql`，PRAGMA user_version）；仅 SQLite（modernc 纯 Go） |
| 迁移文件 | — | 00001_init ~ 00006_seen_hashes（6 个；00003 仅推进版本号、无 DDL） |
| 数据模型 | 无 app_settings/seen_hashes | 新增 app_settings（auth KV）、seen_hashes（永久去重 tombstone）、targets.tags_map、strategies 磁盘/低速率监控字段 |
| 状态机 | 种子 6 态 | 种子 9 态（discovered/retry 内部态），记录 8 态不变（见 BIZ-SPEC §5） |
| 会话 | 内存 session + session_secret 签发 | 无状态 HMAC cookie；secret 自动生成并持久化 app_settings |
| qB 反代 | `/qb/{instance}/` 免鉴权拉起 + WebSocket | 未实现（后端仅服务端直连 qB） |
| 通知 | 7 provider（后三个预留）+ 同实例同 tier 聚合 + digest | 7 provider 全部实现；聚合粒度实例×tier×事件；digest 未实现 |
| API 端点 | setup 分步 / retry·retire·skip / manual / dispatch / pause·resume / preview / logs / import | 收敛为 §10 实际路由（resend 取代 retry/republish；events 取代 logs；restore 取代 import） |
| 前端页面 | Login/Setup/Dashboard/Seeds/QB/Config/Notify/Logs/Preview/Backup | Login + Setup + 9 主页面（Config 拆为 Sources/Targets/Strategy，Logs→Events，Preview 移除） |
| 打包 | pnpm build | npm ci + npm run build |

验收注记：§10 与 `backend/internal/server/server.go`、`internal/api/deps.go`、`internal/api/ops_seeds.go` 逐条一致；§6 与 `backend/internal/store/migrations/00001~00006` 一致。
