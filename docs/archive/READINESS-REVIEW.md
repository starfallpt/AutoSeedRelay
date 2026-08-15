# AutoSeedRelay 部署就绪审查报告

**审查日期**: 2026-08-12
**分支**: main
**审查范围**: 文档、配置、CI/CD、Docker 部署、项目结构

---

## 总览

| 类别 | 评级 | 关键问题数 |
|------|------|-----------|
| 文档 | 需改进 | 3 |
| 配置 | 需改进 | 4 |
| 部署 | 需改进 | 5 |
| CI/CD | 需改进 | 3 |
| 项目结构 | 需改进 | 4 |

---

## 一、严重问题（必须在首次发布前修复）

### 1.1 关键源文件未提交到 Git

`internal/web/auth.go` 和 `internal/web/templates/login.html` 是**未跟踪的新文件**，未包含在任何提交中。

- `server.go` 在第 216-218 行引用了 `s.handleLoginPage`、`s.handleLogin`、`s.handleLogout`，这些方法定义在 `auth.go` 中。
- `server.go` 在第 52 行创建 `sessionStore` 和在第 53 行创建 `rateLimiter`，这两个类型定义在 `auth.go` 中。
- `server.go` 在第 200 行引用 `templates/login.html`。

**影响**：克隆仓库后 `go build` 会编译失败，Web 登录功能完全不可用。

**修复**：立即提交 `auth.go` 和 `login.html`。

### 1.2 6 个文件存在未提交的修改

以下文件处于修改但未暂存状态，且这些修改是当前功能正常运行所必需的：

| 文件 | 变更量 |
|------|--------|
| `config/relay.yaml.example` | +1 行 |
| `internal/config/relay_config.go` | +8 行 |
| `internal/web/server.go` | +78/-27 行 |
| `internal/web/static/app.js` | +27 行 |
| `internal/web/static/style.css` | +24 行 |
| `internal/web/templates/layout.html` | +5 行 |

**影响**：`git clone` 后得到的代码是旧版本，与当前设计不一致。

**修复**：提交所有修改。

### 1.3 go.mod 模块路径与仓库 URL 不匹配

`go.mod` 声明模块为 `github.com/autoseedrelay/go-relay`，但 README 中的克隆地址是 `github.com/starfallpt/AutoSeedRelay`。

**影响**：`go get`/`go install` 会失败，外部包无法导入此模块。

**修复**：将 `go.mod` 中的模块路径统一为 `github.com/starfallpt/AutoSeedRelay`，或反之。

### 1.4 docker-compose.yml 不包含 qBittorrent 服务

`start.sh` 和 README 声称"全套部署"模式会启动 relay + qBittorrent，但 `docker-compose.yml` 只定义了 `autoseedrelay` 一个服务，没有任何 qB 服务定义。

**影响**：用户按文档执行 `bash start.sh`（默认 full 模式）后，relay 找不到 qBittorrent，无法下载种子。

**修复**：在 `docker-compose.yml` 中添加 qBittorrent 服务定义，或修改文档说明 full 模式需要额外的 qB 部署步骤。

### 1.5 docker-compose.external.yml 缺少配置文件挂载

外部 qB 模式的 compose 文件完全没有挂载 `config/` 目录，容器内无法读取 `relay.yaml` 配置文件。

```yaml
# 当前：无配置挂载
services:
  relay:
    build: .
    volumes:
      - ./data:/data
```

**修复**：添加 `- ./config:/app/config:ro` 挂载。

---

## 二、高优先级问题（建议首次发布前修复）

### 2.1 缺少 .dockerignore 文件

没有 `.dockerignore` 会导致整个仓库目录（包括 `.git/`、`data/`、`docs/`、`*.torrent` 等）全部发送到 Docker 构建上下文。

**影响**：构建缓慢，缓存命中率低，可能意外将敏感数据（如 data/ 中的本地数据库）打入镜像。

**建议内容**：
```
.git/
data/
*.torrent
.env
*.log
*.exe
docs/
.pytest_cache/
README.md
LICENSE
```

### 2.2 .gitignore 覆盖不足

当前 `.gitignore` 仅包含 3 条规则：`data/`、`*.torrent`、`.env`。

**缺失项**：
- Go 二进制文件：`relay`、`relay.exe`、`*.exe`、`*.test`
- IDE 文件：`.idea/`、`.vscode/`、`*.swp`、`*.swo`
- 操作系统文件：`.DS_Store`、`Thumbs.db`
- 本地配置：`config/relay.yaml`、`config/local.*`（config.go 错误消息中提到 `config/local.*` 应被 gitignore 但实际未配置）
- 覆盖率输出：`*.out`、`coverage.txt`
- 临时文件：`*.tmp`、`*.bak`

### 2.3 API 文档缺失

Web API 共 8 个端点（见 `server.go` 的 `registerRoutes()`），但没有任何文档描述端点路径、请求参数、响应格式。

**缺失的 API 文档**：

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/status` | GET | 引擎运行状态 |
| `/api/seeds` | GET | 种子列表（支持 status/target/q/page/limit 参数） |
| `/api/seeds/:id` | GET | 种子详情 |
| `/api/seeds/:id/retire` | POST | 撤种 |
| `/api/seeds/:id/retry` | POST | 重试 |
| `/api/config` | GET/POST | 获取/更新配置 |
| `/api/logs` | GET | 日志（支持 level/n 参数） |
| `/api/login` | POST | 登录 |
| `/api/logout` | POST | 登出 |

**修复**：创建 `docs/API.md`，列出所有端点、请求参数、认证方式和响应示例。

### 2.4 配置文件注释不完整

`config/relay.yaml.example` 中的字段缺少逐项说明。以下几个配置块的字段完全无注释：

- `sources[]` 和 `targets[]`：没有说明每个站点对象支持哪些字段
- `strategy`：`role` 和 `promotions` 的可选值未说明
- `monitor`：`low_speed_kbps`、`low_speed_duration`、`low_speed_action`、`site_backoff_*` 字段无解释
- `retire`：各字段的触发逻辑未说明
- `web`：`listen_addr` 格式未说明

README 中的配置示例使用的是旧格式（`source`、`filter`、`qbittorrent`），与当前 `relay.yaml.example` 的格式（`sources`、`targets`、`qb`、`strategy`）不一致。

### 2.5 部署文档与实际配置不一致

`docs/DEPLOY.md` 列出的端口表包含 56921（BT 协议），但 `docker-compose.yml` 和 `docker-compose.external.yml` 都未暴露此端口。

`docs/DEPLOY.md` 第 72 行的目录结构中 `/data/logs/` 路径存在，但 Dockerfile 中 `mkdir -p /app/data /app/logs` 将日志放在 `/app/logs`，而非 `/data/logs`。

### 2.6 Web 面板使用明文密码存储

`internal/config/relay_config.go` 中 `WebConfig.Password` 的默认值为 `"admin"`，密码以明文存储在 YAML 配置中（仅在保存时掩码为 `***`）。

**影响**：配置文件泄漏会直接暴露管理面板密码。

**建议**：支持 bcrypt 哈希存储，在配置中存储哈希值而非明文。

---

## 三、中优先级问题

### 3.1 CI 工作流覆盖不足

当前 `.github/workflows/docker.yml`：

- **仅在 push 到 main 时触发**，没有 PR（`pull_request`）触发器
- **无测试矩阵**（仅 Go 1.22）
- **无代码检查**：未运行 `go vet`、`golangci-lint`、`go fmt`
- **无竞态检测**：测试未使用 `-race` 标志
- **无安全扫描**：未集成 Trivy/Grype/Docker Scout
- **无标签触发**：无法通过 Git 标签触发版本发布

**建议改进**：
```yaml
on:
  push:
    branches: [main]
    tags: ['v*']
  pull_request:
    branches: [main]
```

### 3.2 测试覆盖率极低

765 行测试代码仅覆盖 17 个包中的 7 个：

| 有测试的包 | 无测试的包 |
|-----------|-----------|
| descr, detail, mteam, source, targets, titler (含 python_compare) | bencode, config, engine, parser, pipeline, qb, store, strategy, web |

**缺失测试的关键包**：
- `config`：配置加载、验证、环境变量替换逻辑复杂但无测试
- `store`：SQLite 操作是核心功能，无任何测试
- `engine`：主循环逻辑，无测试
- `web`：HTTP 处理器和认证中间件，无测试

### 3.3 docker-compose 缺少健康检查

两个 compose 文件均未定义 `healthcheck`，无法判断服务是否真正就绪。

**建议**：
```yaml
healthcheck:
  test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:9020/api/status"]
  interval: 30s
  timeout: 5s
  retries: 3
  start_period: 10s
```

### 3.4 容器仅暴露 9020 端口

docker-compose 仅暴露 9020（Web 面板），但文档提及还需要 9021（qB WebUI）和 56921（BT P2P）。如果使用全套部署模式（内置 qB），这些端口必须暴露。

### 3.5 Web 服务器缺少安全加固

- 无 CORS 头配置
- 无 TLS/HTTPS 支持选项
- Cookie 缺少 `Secure` 属性（仅设置了 `HttpOnly` 和 `SameSite`）
- 无请求体大小限制
- Session 存储在内存中（重启后全部失效）
- 仅登录端点有速率限制，其他 API 端点无限制
- 无 CSP（Content-Security-Policy）头
- 无 CSRF token（登录虽为 JSON POST，但 Cookie-based session 建议加上）

### 3.6 config/ 中存在两个配置加载逻辑

`config.go` 中的 `LoadConfig()` 和 `relay_config.go` 中的 `LoadAppConfig()` 是两套独立的配置加载路径：

- `LoadConfig()` 返回 `*RelayConfig`（旧格式：sources/targets 站点列表）
- `LoadAppConfig()` 返回 `*AppConfig`（新格式：包含 QB/Strategy/Retire/Monitor/Web）

这两套逻辑存在于同一 package 中，各自有独立的环境变量替代逻辑，容易引起混淆和维护困难。

---

## 四、低优先级改进建议

### 4.1 缺少 Makefile

没有标准化的构建入口。开发者需记忆原始 Go 命令。

**建议添加**：
```makefile
.PHONY: build test lint docker-build run

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git describe --tags --always)" -o relay ./cmd/relay/

test:
	CGO_ENABLED=0 go test -race -cover ./...

lint:
	golangci-lint run ./...

docker-build:
	docker build -t autoseedrelay:latest .
```

### 4.2 Dockerfile 改进建议

- 运行基础镜像 `alpine:3.20` 可考虑升级到 `alpine:3.21`
- 未注入构建版本信息（git hash/版本号）到二进制
- `VOLUME /data` 与 docker-compose 中的 `./data:/data` 绑定挂载不一致；通常 Dockerfile 中的 VOLUME 应指向容器内路径，而非宿主机路径
- Dockerfile 中 `${TZ}` 环境变量硬编码为 `Asia/Shanghai`，国际用户可能需修改

### 4.3 缺少依赖更新配置

无 Dependabot 或 Renovate 配置，依赖版本安全更新无自动化。

### 4.4 文档改进

- `docs/python-consolidation.md` 在 Go 项目中显得突兀，建议归档或说明其与当前 Go 版本的关系
- 缺少 CHANGELOG.md
- 缺少 CONTRIBUTING.md（贡献指南）
- README 中缺少故障排查（Troubleshooting）章节
- README 中缺少环境变量完整列表（QBHOST/QBUSER/QBPASS/AUTOSEED_* 等）

### 4.5 缺少 .editorconfig

没有 `.editorconfig` 文件，不同编辑器可能产生不一致的缩进和换行符。

### 4.6 start.sh 安全隐患

`start.sh` 在 full 模式下直接调用 `docker compose up -d` 不指定文件。`docker-compose.yml` 中如果 future 添加敏感默认值，可能造成安全隐患。建议始终显式指定 compose 文件。

---

## 五、检查清单汇总

| # | 检查项 | 状态 | 问题数 |
|---|--------|------|--------|
| 1 | README：安装/使用/配置/开发说明 | 基本完整 | 2（配置示例不一致/无环境变量列表） |
| 2 | LICENSE：MIT 许可证 | 通过 | 0 |
| 3 | API 文档 | **缺失** | 1（无 API.md） |
| 4 | 配置文档：relay.yaml 字段注释 | 不完整 | 2（字段缺注释/README 示例过时） |
| 5 | 部署文档：Docker/Compose 步骤 | 部分正确 | 4（缺 qB 服务/端口不一致/目录路径不一致） |
| 6 | Dockerfile：多阶段构建 | 基本正确 | 3（缺 .dockerignore/缺版本注入/alpine 可升级） |
| 7 | docker-compose：服务/volume/port | **不完整** | 5（缺 qB 服务/缺配置挂载/缺端口/缺健康检查） |
| 8 | CI：workflow 正确性 | 基本正确 | 3（缺 PR 触发/缺 lint/缺安全扫描） |
| 9 | .gitignore：覆盖范围 | **不足** | 6+ 种缺失模式 |
| 10 | 项目结构：目录/包命名 | 良好 | 0 |

---

## 六、修复优先级路线图

### 立即修复（阻断发布）
1. 提交 `auth.go` 和 `login.html`
2. 提交所有未提交的修改
3. 修正 `go.mod` 模块路径
4. 修复 `docker-compose.yml`（添加 qB 服务或修正文档）
5. 修复 `docker-compose.external.yml`（添加 config 挂载）

### 发布前建议修复
6. 创建 `.dockerignore`
7. 完善 `.gitignore`
8. 创建 `docs/API.md`
9. 更新 `relay.yaml.example` 注释和 README 配置示例
10. 对齐 DEPLOY.md 与实际 compose 文件
11. 为 Web 面板密码添加哈希支持

### 发布后尽快改进
12. 扩展 CI 工作流（PR 触发、lint、安全扫描）
13. 补充核心包的测试（config、store、engine、web）
14. 添加 docker-compose 健康检查
15. 添加 Web 安全加固（CORS、Secure cookie、CSP）
16. 统一配置加载逻辑

### 长期优化
17. 添加 Makefile
18. 创建 CHANGELOG.md、CONTRIBUTING.md
19. 添加 Dependabot 配置
20. 添加 `.editorconfig`
