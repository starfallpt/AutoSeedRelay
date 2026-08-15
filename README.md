# AutoSeedRelay

PT 自动辅种/转种工具：监听源站 RSS → 清洗种子 → 发布到多个目标站 → 跨站辅种，全程自动 + Web 面板管理。

> **状态：v4 重构中**。当前代码库正在从旧版（Go 原生 web）重写为 **Go(Gin) + Vue3(Element Plus)** 新架构。
> 旧版代码与文档已归档至 `archive/` 与 `docs/archive/`。生产使用请等待重构完成或使用旧版镜像。

## 功能

- 源站 RSS 监听，自动发现新种
- 多目标站发布（NexusPHP API / NexusPHP classic 表单 / M-Team 架构，按站适配）
- 标题、简介、标签、分类自动映射与降级策略
- 多 qBittorrent 实例管理，支持跨站辅种（cross-seed）
- 自动撤种（做种人数/时长双条件，可配置）
- 失败重试队列 + 手动重发
- 通知系统（Webhook / Telegram / SMTP / ntfy / Gotify / Server酱 / PushPlus，分级路由 + 聚合）
- 配置备份导出 / 一键恢复
- Web 面板：状态总览、任务流、事件流、7 天趋势、站点/qB/策略配置

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

或使用 `deploy/docker-compose.yml`。首次启动后打开 `http://<主机>:9020` 完成初始化向导（站点 / qB / 策略配置）。

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

## 文档

- [docs/BIZ-SPEC.md](docs/BIZ-SPEC.md) — 业务规格（实体、流程、规则）
- [docs/ARCHITECTURE-v4.md](docs/ARCHITECTURE-v4.md) — 技术架构（模块、数据模型、API、迁移计划）
- [docs/archive/](docs/archive/) — 旧版历史文档（站点适配参考等）

## 安全

- 站点/qB 凭据 AES-256-GCM 加密落库，主密钥由环境变量 `AUTOSEED_MASTER_KEY` 提供（未设置时自动生成于 `data/master.key`）
- 面板登录会话鉴权；API 响应中凭据一律脱敏
- 旧版已知问题（硬编码口令等）已在新架构中修复；**如你仍在使用旧版部署，请立即轮换相关口令**

## License

[MIT](LICENSE)
