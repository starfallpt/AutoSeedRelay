# AutoSeedRelay 重构架构方案 v4

> 状态：已与用户逐项确认（业务定稿见 docs/BIZ-SPEC.md）。本文件是重构实施的架构依据。
> 配套：docs/BIZ-SPEC.md（业务语义权威）、docs/ARCHITECTURE-v3.md（旧版，仅历史参考）。

## 1. 目标与原则

- **目标**：重写为 Go(Gin) + Vue3 单容器应用，实现 BIZ-SPEC 全部业务语义，修复旧版 P0 安全清单与 23 条业务缺陷。
- **原则**：业务语义以 BIZ-SPEC 为准；旧代码仅作移植参考（archive/）；能复用逻辑的移植+修复，不复用结构；安全修复不进补丁、直接做进新架构。

## 2. 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.22+ · Gin · sqlc · goose 迁移 · yaml.v3（部署级配置） |
| 存储 | SQLite 默认（modernc 纯 Go）/ PostgreSQL 可选（DSN 切换，sqlc 双方言） |
| 前端 | Vue 3 · Vite · TypeScript · Element Plus · Pinia · axios |
| 打包 | 多阶段 Docker：node 编前端 → Go embed 产物 → alpine 单容器 |
| 测试 | 单测 SQLite / 集成 PG 容器 / race / gosec / trivy |

## 3. 工程结构（monorepo）

```
AutoSeedRelay/
├─ backend/
│  ├─ cmd/relay/main.go        # 唯一入口 serve
│  ├─ internal/
│  │  ├─ config/               # 部署级配置（viper）：端口/DSN/日志/目录
│  │  ├─ secret/               # 主密钥 + AES-256-GCM 加解密 + 脱敏
│  │  ├─ store/                # sqlc 生成 + 仓储接口（SQLite/PG 双引擎）
│  │  ├─ qb/                   # qB 客户端 + 多实例连接池 + 免鉴权反代支持
│  │  ├─ source/               # RSS/详情/下载（移植+加固：大小上限/SSRF防护/退避）
│  │  ├─ adapters/             # 目标站适配（nexusphp/classic/mteam）+ 站点枚举探测
│  │  ├─ bencode/              # 移植+加固（溢出/深度/大小限制）
│  │  ├─ titler/ descr/        # 移植+修正（去 Python 怪癖固化）
│  │  ├─ pipeline/             # RelayOne v2：下载→清洗→发布/辅种（校验降级）
│  │  ├─ engine/               # 编排：Poller/Monitor/Dispatcher/RetryQueue
│  │  ├─ notifier/             # 通知：provider→实例→分层路由→聚合
│  │  ├─ backup/               # zip 导出/导入恢复
│  │  ├─ server/               # Gin 装配、路由、依赖注入
│  │  ├─ api/                  # handlers：auth/setup/seeds/qb/config/notify/preview/backup/logs/dashboard
│  │  └─ webfs/                # embed 前端构建产物 + /qb/* 反向代理
│  ├─ migrations/              # goose：sqlite/ 与 postgres/ 双方言目录
│  └─ queries/                 # sqlc .sql 源（共享语义，方言同目录）
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
        category_overrides JSON, dimension_overrides JSON, enc_* 凭据, status)
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
           image_cover_enabled, retry_max)
```

## 7. 引擎设计

- **Poller**：每源站独立 ticker（poll_interval，启动立即一轮）；RSS→筛选→去重→入队。
- **Dispatcher**：多 qB 分派接口 `PickQB(kind)`；策略 priority（手动优先级降序）/ least_jobs / most_free_disk / round_robin；交叉辅种优先同 qB。
- **RetryQueue**：内存延时队列（退避 60s/300s/900s，上限 retry_max=3）；启动时从 failed 且未达上限的记录重建；失败进 failed + critical 通知（重试耗尽）。
- **Monitor**：遍历全部启用 qB：连接状态/做种统计/真实磁盘/低速率中止/撤种判定（seeders≥10 或 minutes>60，AND/OR 可配，ratio 默认关）；0 进度副本不计时长。
- **状态机**：种子级 6 态 + 记录级 8 态（BIZ-SPEC §5）；种子终态聚合规则：全部记录终态 + 无副本。
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

## 9. 通知系统

- Provider 注册表接口 `Send(ctx, instance, msg)`；内置 webhook/telegram/smtp/ntfy/gotify/serverchan/pushplus（后三个预留实现）。
- 实例：同 provider 可多份；notifier_routes 勾选矩阵（实例×tier）。
- 聚合：同实例同 tier 10 分钟合并；critical 直通不合并；每日 digest 可选（info 层）。
- **聚合补充（Vertex 借鉴）**：错误类事件另设「次数阈值 + 周期清零」熔断（如 1 小时内同类错误 >20 条暂停该事件）；Telegram 对进行中任务状态用 `editMessageText` 复用同一条消息防刷屏。
- 入站 webhook（预留）：apiKey 放路径、独立于 session 鉴权（外部系统反向触发）。
- 事件 tier：critical（发布失败重试耗尽/qB 全断/磁盘 critical/cookie 过期）；warning（磁盘 low/单 qB 断连）；info（发布成功/降级辅种/自动撤种/digest）。
- 全部默认关；测试发送按钮。

## 10. API 契约 v2（base /api/v2，除标注外均需 session + CSRF）

| 域 | 端点 |
|---|---|
| 鉴权 | POST /auth/login · POST /auth/logout · GET /auth/me |
| 向导 | GET /setup/status · POST /setup/qb|source|targets|complete（**仅未初始化开放**） |
| 仪表盘 | GET /dashboard（聚合：状态条+统计卡+进行中任务+事件流+7天趋势） |
| 种子 | GET /seeds（筛选分页 SQL）· GET /seeds/{id}（含 records+replicas+log）· POST /seeds/{id}/retry|retire|skip · POST /seeds/{id}/republish（从失败点恢复）· POST /seeds/manual（multipart .torrent / URL+磁链 + target_ids） |
| qB | GET/POST/PUT/DELETE /qb（实例 CRUD）· POST /qb/{id}/test · GET/PUT /qb/dispatch |
| 站点 | CRUD /sources /targets · POST /targets/{id}/pause|resume|probe（枚举探测） |
| 策略 | GET/PUT /strategies |
| 通知 | CRUD /notify/instances · GET/PUT /notify/routes · POST /notify/{id}/test |
| 预览 | GET /preview/fetch · GET /preview/seed · POST /preview/compare |
| 日志 | GET /logs?level=&search=&page=（后端搜索，结构化） |
| 备份 | GET /backup/export（zip 下载）· POST /backup/import（multipart zip，恢复前自动备份当前库） |
| 代理 | /qb/{instance}/*（免鉴权拉起，见 §8） |

**中间件链**：Recovery → RequestID+结构化日志 → CSRF（自定义头+双提交）→ 限流（IP，可信代理可配）→ Session（内存，session_secret 签发）→ SetupGuard。

## 11. 前端设计

- 页面：Login / Setup（四步向导）/ Dashboard（五区）/ Seeds（抽屉详情）/ QB（多实例+分派）/ Config（站点+策略）/ Notify（实例+路由矩阵）/ Logs / Preview（字段对比）/ Backup（导出+恢复）。
- stores：auth / seeds / qb / config / notify；api/ 下 axios 封装 + TS 类型（与后端契约对齐，契约由 Go struct 生成或手工同步）。
- 图表轻量自绘（趋势柱状），不引重图表库；Element Plus 按需引入。

## 12. Docker 打包（单容器）

```
stage1 node:20-alpine   → pnpm build 前端 dist/
stage2 golang:1.22-alpine → embed dist/ 编 relay 静态二进制（CGO_ENABLED=0）
stage3 alpine → relay + data/ 卷 + healthcheck（HTTP /api/v2/health）
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
