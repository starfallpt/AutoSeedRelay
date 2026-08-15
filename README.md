# AutoSeedRelay

PT 自动辅种/转种工具：监听源站 RSS → 清洗种子 → 发布到多个目标站 → 跨站辅种，全程自动 + Web 面板管理。

> **状态：v4 重构 M0~M3 已完成**——Go(Gin) + Vue3(Element Plus) 新架构已可用（Web 面板、鉴权、通知、备份齐全）；M4 生产站验证 / M5 收尾进行中。
> 旧版代码与文档已归档至 `archive/` 与 `docs/archive/`。

## 功能

- 源站 RSS 监听，自动发现新种
- 多目标站发布（NexusPHP API / NexusPHP classic 表单 / M-Team 架构，按站适配）
- 标题、简介、标签、分类自动映射与降级策略
- 多 qBittorrent 实例管理，支持跨站辅种（cross-seed）
- 自动撤种（做种人数/时长双条件，可配置）
- 失败重试队列 + 手动重发
- 通知系统（Webhook / Telegram / SMTP / ntfy / Gotify / Server酱 / PushPlus，分级路由 + 聚合 + 熔断）
- 配置备份导出 / 一键恢复
- Web 面板：初始化向导、登录鉴权、仪表盘、站点/qB/策略/通知配置、事件流、备份

### 面板页面（M3）

| 页面 | 功能 |
|---|---|
| 初始化向导 / 登录 | 首次启动设置面板密码（`/setup`），之后登录进入面板 |
| 仪表盘 | 状态条（qB/源站/磁盘/运行时长）、统计卡、进行中任务、事件流、7 天趋势 |
| 种子 | 种子列表 + 详情抽屉（记录/副本/日志），手动重发 |
| 事件 | 活动日志（分页/筛选） |
| 站点源 / 目标站 | 源站与目标站配置（凭据加密、连接测试、枚举探测） |
| qB 实例 | 多 qB 管理（增删改 + 连接测试） |
| 策略 | 筛选 / 撤种 / 分派 / 时区 / 图床 / 磁盘·低速率监控阈值 |
| 通知 | 通知实例 + 实例×tier 路由矩阵 + 测试发送 |
| 备份 | 一键导出 zip / 上传恢复（重启后生效） |

## 快速开始（Docker）

```bash
# 配置面板密码与加密主密钥（64位hex，留空自动生成）
cat > .env <<'EOF'
AUTOSEED_WEB_PASSWORD=你的面板密码
AUTOSEED_MASTER_KEY=
EOF

docker run -d --name autoseedrelay \
  -p 9020:9020 \
  -v autoseedrelay-data:/data \
  --env-file .env \
  ghcr.io/starfallpt/autoseedrelay:latest
```

或使用 `deploy/docker-compose.yml`。首次启动后打开 `http://<主机>:9020`，先经初始化向导设置面板密码，再登录面板配置站点 / qB / 策略。

> qBittorrent 需单独部署（任意 qB 4.x/5.x），面板内添加其连接信息即可。

## 开发

```bash
# 后端（需 Go 1.22+）
cd backend
go run ./cmd/relay serve          # 默认 :9020

# 前端（需 Node 20+）
cd frontend
npm install
npm run dev                       # 代理 /api → localhost:9020
```

CI：GitHub Actions（`.github/workflows/ci.yml`）执行测试、类型检查、多阶段镜像构建并发布到 GHCR。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `AUTOSEED_WEB_PASSWORD` | 首启建议 | 面板初始密码（bcrypt 哈希落库）。**仅在首次启动且尚无密码哈希时消费一次**，之后 DB 内哈希为准 |
| `AUTOSEED_MASTER_KEY` | 否 | 凭据加密主密钥（64 位 hex）。留空则首启自动生成于 `/data/master.key`（0600） |
| `AUTOSEED_LISTEN_ADDR` | 否 | HTTP 监听地址，默认 `:9020` |
| `AUTOSEED_LOG_LEVEL` | 否 | `debug`/`info`/`warn`/`error`，默认 `info` |
| `AUTOSEED_DB_PATH` | 否 | SQLite 数据库路径，默认 `data/relay.db` |
| `AUTOSEED_PROXY` | 否 | 源站直连下载（direct 模式）的 HTTP 代理 |

> 会话签名密钥**无独立环境变量**（不存在 `AUTOSEED_SESSION_SECRET`）——首启自动生成并持久化于数据库 `app_settings` 表（`session_secret` 键）。业务配置（站点/qB/策略/通知）全部存 DB，不再有站点级 `AUTOSEED_<SITE>_<FIELD>` 环境变量。

## 升级

- 同版本内更新：`docker compose pull && docker compose up -d`——数据卷 `relay-data` 持久化，`/data/relay.db` 与 `/data/master.key` 不受影响。
- schema 迁移随启动自动应用（`PRAGMA user_version` 追踪，向前兼容）。
- 跨版本升级前建议先在面板「备份」页导出 zip（数据库 + 脱敏配置）；恢复时上传 zip 后**重启容器**生效。
- 主密钥 `master.key` 一经生成请妥善备份：丢失将无法解密已加密的站点/qB/通知凭据。

## 文档

- [docs/BIZ-SPEC.md](docs/BIZ-SPEC.md) — 业务规格（实体、流程、规则）
- [docs/ARCHITECTURE-v4.md](docs/ARCHITECTURE-v4.md) — 技术架构（模块、数据模型、API、迁移计划）
- [docs/archive/](docs/archive/) — 旧版历史文档（站点适配参考等）

## 安全

- 站点/qB/通知凭据 AES-256-GCM 加密落库，主密钥由环境变量 `AUTOSEED_MASTER_KEY` 提供（未设置时自动生成于 `data/master.key`）
- 面板登录：bcrypt 口令 + 无状态 HMAC 会话（24h）+ CSRF 双提交 + 登录限流（每 IP 每分钟 5 次）
- API 响应中凭据一律脱敏 `***`；错误/通知正文剥离内嵌凭据
- 出站请求 SSRF 加固（拒私网/环回）+ 响应体大小上限
- 旧版已知问题（硬编码口令等）已在新架构中修复；**如你仍在使用旧版部署，请立即轮换相关口令**

## License

[MIT](LICENSE)
