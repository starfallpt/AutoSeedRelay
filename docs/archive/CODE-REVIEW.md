# AutoSeedRelay Go 代码审查报告

**审查日期**: 2026-08-12
**审查范围**: 全量 Go 源码 (40 个 .go 文件, 14 个 internal 包 + 2 个 cmd 入口)
**工具辅助**: go vet, go build, go test, 人工分析

---

## 严重 (必须修复 — 会导致运行时错误或数据损坏)

### CRIT-1: store/migrate.go -- 数据库迁移从未被调用

**文件**: `internal/store/store.go:88-114` (Open 函数)
**文件**: `internal/store/migrate.go:17` (migrate 方法)

`RelayStore.migrate()` 方法定义了 v0->v1 迁移 (创建 `seeds`、`relay_records`、`activity_log` 三张表), 但在 `Open()` 函数中从未调用 `s.migrate()`。 这导致 `seeds.go`、`records.go`、`history.go` 中所有操作这些表的方法在执行时都会因表不存在而失败。

**后果**: 
- `RelayStore.InsertSeed()` → "no such table: seeds"
- `RelayStore.InsertRecord()` → "no such table: relay_records"
- `RelayStore.LogActivity()` → "no such table: activity_log"

**修复**: 在 `Open()` 返回前添加 `s.migrate()` 调用, 或直接在 `Open()` 内将 `seeds`、`relay_records`、`activity_log` 的 DDL 加入初始化。

### CRIT-2: store/records.go:292 -- setRecordColumn 无互斥锁保护

**文件**: `internal/store/records.go:292-303`

```go
func (s *RelayStore) setRecordColumn(id int64, col string, val interface{}) error {
    db, err := s.requireOpen()  // 无 s.mu.Lock()
    ...
}
```

该 store 内所有其他公开方法 (`InsertRecord`、`GetRecordBySeedTarget`、`UpdateRecordStatus` 等) 都先获取 `s.mu.Lock()`, 唯独 `setRecordColumn` 未加锁。 当前该方法未被外部调用, 但一旦使用即存在并发写 race condition。

**修复**: 调用方 (或其他包) 不应直接调用未加锁的内部方法; 或将此方法改为在调用处加锁 (或直接删除 — 它目前未被使用, 是死代码)。

---

## 警告 (应尽快修复 — 影响正确性或一致性)

### WARN-1: pipeline.go 使用 stdlib log 而非 slog

**文件**: `internal/pipeline/pipeline.go` (共 14 处)

```go
import "log"  // 应使用 "log/slog"

log.Printf("store.MarkStatus(%s, %s) 失败: %v", infoHash, status, err)
```

所有其他包 (engine、web、serve) 均使用结构化日志 `log/slog`, pipeline 是唯一例外。 这导致 pipeline 的输出无法被集中日志系统采集, 且格式不一致 (缺少时间戳、日志级别、结构化字段)。

**修复**: 将 `log.Printf` 替换为 `slog.Warn` / `slog.Error` / `slog.Info`, 并移除 `"log"` import。

### WARN-2: engine/engine.go:348-373 -- SaveConfig 并非并发安全

**文件**: `internal/engine/engine.go:348-373`

```go
func (e *Engine) SaveConfig(cfg *config.AppConfig) error {
    e.cfg = cfg  // 直接赋值,未加锁
    ...
    e.filter = strategy.NewFilter(...)  // 直接赋值,未加锁
    e.retire = strategy.NewRetirePolicy(...)
}
```

`e.cfg`、`e.filter`、`e.retire` 被多个 goroutine 并发读取 (cycleLoop、monitorLoop、web handlers), 但 SaveConfig 直接写入这些字段未加锁, 造成 data race。 `go test -race` 可触发此问题。

**修复**: 使用 `sync.Mutex` 保护这三个字段的读写, 或用 `atomic.Value` 存储不可变快照。

### WARN-3: engine/monitor.go:125 -- 磁盘空间为硬编码常量

**文件**: `internal/engine/monitor.go:124-126`

```go
e.stats.DiskTotalGB = 500.0  // 硬编码,不读取实际磁盘容量
e.stats.DiskFreeGB = e.stats.DiskTotalGB - float64(totalSize)/(1024*1024*1024)
```

`DiskTotalGB` 被硬编码为 500.0, 而非调用 qB API 获取真实值。 `DiskFreeGB` 同样基于此假值计算, 导致磁盘告警阈值 (`DiskLowGB`/`DiskCriticalGB`) 失效。

**修复**: 通过 `GetDiskSpace()` 或 qB `/api/v2/sync/maindata` 获取真实 `free_space_on_disk` 值。

### WARN-4: 多处 MarkStatus 错误被静默忽略

**文件**: `internal/engine/cycle.go` (行 121, 128, 139, 144); `internal/engine/monitor.go` (行 101, 187, 242); `cmd/relay/main.go` (行 468, 473, 483, 492, 522, 527, 531)

约 15+ 处调用 `_ = e.store.MarkStatus(...)` / `_ = s.MarkStatus(...)` 忽略了返回 error。 若 SQLite 写入失败 (如磁盘满、DB 损坏、表不存在), 状态更新会静默丢失, 且无告警。

**修复**: 至少用 `slog.Warn` 记录失败; 对于关键路径 (如 "uploaded" 最终状态), 应将 error 向上传播。

### WARN-5: web/server.go -- Web 面板默认密码为空且明文存储

**文件**: `internal/config/relay_config.go:186` (默认 password = "admin")
**文件**: `internal/config/relay_config.go:362` (环境变量 `AUTOSEED_WEB_PASSWORD`)
**文件**: `internal/web/auth.go:194` (明文比对)

```go
if req.Password != s.cfg.Web.Password {
```

密码 "admin" 作为默认值不安全, 且存储和比对均为明文。 生产环境若未设环境变量 `AUTOSEED_WEB_PASSWORD`, 面板处于弱密码状态。

**修复**: 
- 默认值至少使用随机串, 首次启动时打印
- 存储加盐哈希, 比对使用 `crypto/subtle.ConstantTimeCompare` 防时序攻击
- 无自定义密码时完全禁用面板或强制设置

### WARN-6: engine/engine.go:126-127 -- SourceClient 失败时静默跳过

**文件**: `internal/engine/engine.go:112-118`

```go
sc, err := source.NewSourceClient(sp.RSSURL, source.SourceClientOptions{...})
if err != nil {
    slog.Warn("engine: failed to create source client", "site", sp.Name, "error", err)
    continue  // 跳过该 source, 不加入 srcs map
}
```

若因配置错误导致 source client 初始化失败, engine 仅打 warn 日志并继续。 若所有 source 都失败, engine 无 RSS 可抓但仍启动, 不会报错 —— 用户需人工检查日志才能发现。

**修复**: 若所有 source 创建均失败, `Start()` 应返回错误, 或在 stats 中暴露 `failed_sources` 计数供监控。

### WARN-7: web/auth.go -- Session cookie 缺少安全属性

**文件**: `internal/web/auth.go:203-209`

```go
http.SetCookie(w, &http.Cookie{
    Name:     "session",
    Value:    token,
    Path:     "/",
    HttpOnly: true,
    SameSite: http.SameSiteStrictMode,
    MaxAge:   86400,
    // 缺少 Secure: true  (仅 HTTPS 时发送)
})
```

若通过 HTTP (非 HTTPS) 访问, session cookie 会在网络上明文传输, 可被中间人拦截。 虽然有 HttpOnly 和 SameSite, 但缺少 `Secure` 标志。

**修复**: 增加 `Secure: true` (生产环境应全程 HTTPS); 或通过配置项控制。

### WARN-8: engine/engine.go:273-292 -- GetSeedDetail 以行索引为 ID, 不可靠

**文件**: `internal/engine/engine.go:273-292`

```go
// For now, use row index as ID.
if id < 0 || int(id) >= len(all) {
    return nil, fmt.Errorf("engine: seed %d not found", id)
}
row := all[id]
```

用 `All()` 返回的 slice 索引作为 ID 不可靠: 新种子插入后顺序可能改变, 且 `All()` 调用量随数据增长而线性增长 (无 LIMIT/OFFSET, 全量内存过滤)。

**修复**: 在 `relay_jobs` 表中增加自增主键 INTEGER PRIMARY KEY AUTOINCREMENT, 用真实 ID 做查询和分页。

### WARN-9: source/source.go:344-377 -- 正则每次调用时编译

**文件**: `internal/source/source.go:109,116,348,363`

```go
m := regexp.MustCompile(`id=(\d+)`).FindStringSubmatch(link)  // line 109
site := c.siteBase()  // line 348 → MustCompile inside siteBase
```

`ParseRSS` 和 `siteBase()` 每次调用都编译正则表达式。 高频调用时 (每轮 RSS 20+ 条目、每秒数百次), 应预编译为包级变量。

**修复**: 将 `regexp.MustCompile` 提升为全局 `var` 或 `sync.Once` 初始化。

---

## 建议 (影响代码质量/可维护性/性能, 非阻塞)

### SUGG-1: 测试覆盖缺失严重

当前有测试的包: descr, detail, mteam, source, targets, titler
**无测试的包**: bencode, config, engine, parser, pipeline, qb, store, strategy, web, cmd/relay

10 个包/入口零测试覆盖, 这些恰好是核心业务逻辑 (engine 调度、pipeline 编排、store 持久化、qb 客户端、目标站上传):

| 包 | 风险 | 说明 |
|---|---|---|
| engine | 高 | 调度循环、状态管理无测试 |
| pipeline | 高 | 转种编排 A/B 模式无测试 |
| store | 高 | SQLite CRUD 无单元测试 |
| qb | 高 | qB WebUI API 客户端无 mock 测试 |
| config | 中 | 配置加载/校验无测试 |
| parser | 中 | 种子清洗逻辑无测试 |
| strategy | 中 | 过滤/退休规则纯函数, 易测试但未测 |
| web | 中 | HTTP API + 认证无测试 |
| bencode | 低 | 纯编解码, 风险较低 |
| cmd/relay | 低 | CLI 入口逻辑, 可通过集成测试覆盖 |

### SUGG-2: store/records.go/292 -- setRecordColumn 是死代码

**文件**: `internal/store/records.go:292`

`setRecordColumn` 无任何调用方, 可移除。

### SUGG-3: engine/engine.go:431-432 -- 冗余 import 占位符

**文件**: `internal/engine/engine.go:431-432`

```go
var _ = targets.Upload   // 仅确保 targets 包未被编译器剔除
var _ = source.ParseRSS   // 同上
```

这两个包已在 `cycle.go` (同 package) 中实际使用, 此处占位符不必要——除非 cycle.go 被删除。

### SUGG-4: web/qb_proxy.go:164 -- 未使用的 httputil import

**文件**: `internal/web/qb_proxy.go:164`

```go
var _ = httputil.ReverseProxy{}
```

`net/http/httputil` 被导入但未实际使用 (仅靠占位符避免编译错误)。 如果这也是 "备用导入", 建议移除。

### SUGG-5: parser/parser.go:186 -- 使用 math/rand 进行种子时间抖动

**文件**: `internal/parser/parser.go:186-187`

```go
jitter = low + rand.Intn(high-low+1)
```

`math/rand` 非加密安全, 种子清洗的场景风险极低 (仅用于使种子文件哈希不同), 但若需要更好的不可预测性, 可用 `crypto/rand`。 当前用途可接受, 建议保留但记录。

### SUGG-6: descr/descr.go -- humanSize 函数中 `_ = i` 无意义

**文件**: `internal/descr/descr.go:375`

```go
n /= 1024.0
_ = i
```

变量 `i` 被声明显式丢弃, 说明循环逻辑中未使用索引。 可重构为仅使用 `range units` 而无需 `_ = i`。

### SUGG-7: 错误信息使用中文硬编码

整个代码库中 error 消息和打印输出混用中英文 (如 `"RSS 抓取失败"`, `"未知目标站类型"`, `"种子已存在"`)。 统一为英文可简化国际化、日志搜索和社区贡献。

### SUGG-8: store/Open 未设置 SQLite PRAGMA busy_timeout

**文件**: `internal/store/store.go:92`

```go
db.Exec("PRAGMA journal_mode=WAL")
```

建议同时设置 `PRAGMA busy_timeout=5000` (等待 5s 而非立即返回 SQLITE_BUSY), 在高并发场景下更健壮。

### SUGG-9: go.sum 中包含大量间接依赖

`go.sum` 行数较多 (含 `modernc.org/sqlite` 的 CGo-free SQLite 实现)。 确认 `modernc.org/sqlite` 适合生产环境 (纯 Go SQLite, 无需 CGO), 但性能略低于 CGo SQLite。

### SUGG-10: web 模板缺少 CSP/XSS 保护

**文件**: `internal/web/server.go:283`

```go
w.Header().Set("Content-Type", "text/html; charset=utf-8")
```

Web 面板未设置 `Content-Security-Policy` 头。 建议增加 `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'` 以防御 XSS。

---

## 并发安全分析 (汇总)

| 组件 | 共享状态 | 保护方式 | 安全? |
|---|---|---|---|
| RelayStore (store.go) | *sql.DB, 所有方法 | sync.Mutex | 安全 (除 setRecordColumn) |
| Engine (engine.go) | cfg, filter, retire | 无保护 (直接写) | **不安全** (WARN-2) |
| Engine (engine.go) | stats | sync.RWMutex | 安全 |
| Engine (engine.go) | running | atomic.Bool | 安全 |
| Server (web) | logBuf | sync.Mutex | 安全 |
| Server (web) | sessionStore.sessions | sync.Mutex | 安全 |
| Server (web) | rateLimiter.fails | sync.Mutex | 安全 |
| QBProxy | sid, expiry | sync.Mutex | 安全 |
| QBittorrent (qb.go) | loggedIn, slowTracker | sync.Mutex | 安全 (slowMu) |
| QBittorrent (qb.go) | loggedIn 字段 | 无锁读写 | **不安全** (若多 goroutine 调用 request 可并发修改 loggedIn 状态) |
| SourceClient | qbClient | 无锁 | **不安全** (DownloadViaQB 中 lazy init, 若多个 goroutine 同时调用可能创建多个 client) |

### CRIT-3: qb/qb.go -- QBittorrent.loggedIn 无并发保护

**文件**: `internal/qb/qb.go:76,118-154`

```go
type QBittorrent struct {
    ...
    loggedIn    bool  // 无锁保护
    ...
}

func (q *QBittorrent) Login() error {
    if q.loggedIn {  // 读
        return nil
    }
    ...
    q.loggedIn = true  // 写
}
```

`loggedIn` 被多个 goroutine 并发读写 (monitor 循环、cycle 循环、web 代理), 是 data race。 `go test -race` 会触发。

**修复**: 使用 `sync.Mutex` 或 `atomic.Bool` 保护 `loggedIn` 字段。

### CRIT-4: source/source.go -- SourceClient.qbClient 懒初始化无锁

**文件**: `internal/source/source.go:528-534`

```go
if c.qbClient == nil {  // 读
    client, err := qb.NewQBittorrent(...)
    ...
    c.qbClient = client  // 写
}
```

若多个 goroutine 同时调用 `DownloadViaQB`, 可能创建多个 QBittorrent 实例 (资源泄漏), 且其中一个被丢弃。

**修复**: 使用 `sync.Once` 或 `sync.Mutex` 保护初始化。

---

## 资源泄漏分析 (汇总)

| 位置 | 资源 | 是否关闭 | 备注 |
|---|---|---|---|
| source.go:385 | http.Response.Body | 是 (defer) | Ok |
| source.go:470 | http.Response.Body | 是 (defer) | Ok |
| detail.go:312 | http.Response.Body | 是 (defer) | Ok |
| detail.go:337 | http.Response.Body | 是 (defer) | Ok |
| qb.go:129 | http.Response.Body | 是 (defer) | Ok |
| qb.go:198 | http.Response.Body | 是 (显式 Close) | 重试路径中关闭 |
| qb.go:444 | http.Response.Body | 是 (显式 Close) | 404 回退路径中关闭 |
| qb/client.go:101,125,159,316 | http.Response.Body | 是 (defer) | Ok |
| qb/autoinit.go:94 | http.Response.Body | 是 (显式 Close) | Ok |
| targets/mteam.go:145 | http.Response.Body | 是 (defer) | Ok |
| targets/nexusphp.go:136 | http.Response.Body | 是 (显式 Close + readRespBody) | non-200 路径中关闭 |
| engine.go:80 | store | 是 (显式 Close) | New 失败时回滚 |
| pipeline.go:188 | store (临时创建) | 是 (defer Close) | Ok |
| store.go (All/PendingJobs) | sql.Rows | 是 (defer) | Ok |
| engine/cycle.go:107 | goroutine (processSeed) | **否** | goroutine 启动后无上限控制, 无 wg 跟踪 |

### WARN-10: engine/cycle.go:107 -- goroutine 泄漏风险

**文件**: `internal/engine/cycle.go:107`

```go
go e.processSeed(ih, item, name)
```

每个命中的 RSS 条目启动一个 goroutine, 无并发限制 (max_concurrent 虽在配置中但未实现), 且 `Stop()` 不等待这些 goroutine 完成。 高频 RSS (100+ 条目) 可能导致数百 goroutine 同时运行。

**修复**: 使用带缓冲的 channel 或 semaphore 限制并发数; 或在 `wg` 中跟踪这些 goroutine。

---

## 日志一致性分析

| 包 | 日志库 | 是否一致? |
|---|---|---|
| cmd/relay/serve.go | slog | OK |
| engine | slog | OK |
| web | slog | OK |
| pipeline | **stdlib log** | **不一致** |
| cmd/relay/main.go | fmt.Printf/Fprintln | CLI 输出 (可接受) |
| config | 无直接日志 | OK |

**结论**: pipeline.go 是唯一破坏一致性的包 — 应统一为 slog。

---

## 死代码分析

| 位置 | 函数/变量 | 说明 |
|---|---|---|
| store/records.go:292 | `setRecordColumn` | 零调用方 |
| engine/engine.go:431-432 | `var _ = targets.Upload` | 冗余占位符 |
| web/qb_proxy.go:164 | `var _ = httputil.ReverseProxy{}` | 死代码 |
| descr/descr.go:375 | `_ = i` | 无意义丢弃 |
| engine/engine.go:432 | `var _ = source.ParseRSS` | 冗余占位符 |

---

## Import 循环依赖分析

```text
cmd/relay → pipeline, parser, qb, source, store, targets, config, engine, web
engine → config, qb, source, store, strategy, targets, web
pipeline → bencode, parser, qb, source, store, targets
targets → descr, mteam, parser
web → config
qb → bencode
source → qb
```

依赖图为 DAG (无环), 栈深度最大 3。**无 import 循环问题**。

---

## 总结

| 等级 | 数量 | 最紧急 |
|---|---|---|
| 严重 | 4 | migrate() 未调用 (CRIT-1)、Engine 并发不安全 (CRIT-3, 4) |
| 警告 | 10 | pipeline 日志不一致 (WARN-1)、硬编码磁盘 (WARN-3)、默认弱密码 (WARN-5) |
| 建议 | 10 | 测试覆盖不足 (SUGG-1)、死代码 (SUGG-2,3,4) |

**零测试覆盖的 10 个关键包** (engine, pipeline, store, qb, config, parser, strategy, web, bencode, cmd/relay) 是最大的技术债务来源, 建议在下一阶段按 risk 优先级补充测试。
