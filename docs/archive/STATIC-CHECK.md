# AutoSeedRelay Go 静态检查报告

**检查日期**: 2026-08-12
**检查范围**: `internal/` + `cmd/` 下所有 `.go` 文件
**工具**: `go vet`, 人工代码审查

---

## 1. go vet 检查

**结果: 通过 (无警告)**

```
$ go vet ./internal/... ./cmd/...
(无输出,退出码 0)
```

编译层无未使用 import、无 `printf` 格式错误、无不可达代码等 vet 级问题。

---

## 2. Error 处理

### 2.1 map 类型断言（可接受）

以下文件大量使用 `v, _ := m["key"].(type)` 模式,忽略类型断言失败:

| 文件 | 行号 | 说明 |
|------|------|------|
| `internal/engine/engine.go` | 243-251 | target_status, title, target_site |
| `internal/engine/monitor.go` | 73,100,173,178,182,224,230-232 | state, dlspeed, hash, ratio 等 |
| `internal/qb/client.go` | 214,293,298 | state, progress |
| `internal/qb/qb.go` | 549-552 | progress, completed, completion_on, state |
| `internal/source/source.go` | 555,589-590 | category, hash |
| `internal/store/store.go` | 273 | info_hash |

**评估**: 可接受。类型断言失败时返回零值（`""`/`0`/`false`），在 map 访问场景下零值即 "不存在/未设置" 的合理语义。

### 2.2 strconv.Atoi 错误忽略（低风险）

| 文件 | 行号 | 上下文 |
|------|------|--------|
| `internal/targets/base.go` | 618 | 正则捕获组编号（regex 保证为数字） |
| `internal/targets/base.go` | 693,696 | 季/集号提取（regex 保证为数字） |
| `internal/targets/nexusphp_classic.go` | 179 | details.php?id=N 解析（regex 保证为数字） |
| `internal/titler/titler.go` | 563 | `toInt()` 辅助函数，parse 失败返回 0 |

**评估**: 除 `titler.go:563` 外，其他均由正则保证输入合法。`toInt()` 返回 0 作为 fallback 是可接受的，但调用方需注意无法区分真实的 0 和解析失败的 0。

### 2.3 url.Parse 错误忽略（中等风险）

| 文件 | 行号 | 代码 |
|------|------|------|
| `internal/qb/qb.go` | 124 | `u, _ := url.Parse(q.host)` |
| `internal/web/qb_proxy.go` | 74 | `if u, _ := url.Parse(p.qbURL); u != nil {` |

**问题**: 若 `q.host` 或 `p.qbURL` 格式非法，`url.Parse` 返回 nil。qb_proxy.go 有 nil 检查，但 qb.go:124 的 `SID()` 方法直接使用 `u` 传给 `Cookies(u)`，若为 nil 则可能触发 nil pointer dereference。

**建议**: 在 `NewQBittorrent` 构造时验证 host 格式，或 `SID()` 中增加 nil 检查。

### 2.4 io.ReadAll 错误忽略（低风险--错误路径）

分布在以下文件的非 200 响应处理中:

| 文件 | 行号 |
|------|------|
| `internal/detail/api.go` | 235 |
| `internal/qb/autoinit.go` | 121 |
| `internal/qb/client.go` | 102,126,160,317 |
| `internal/qb/qb.go` | 147,272,287,396,432 |
| `internal/targets/mteam.go` | 147 |
| `internal/web/setup.go` | 578 |

**评估**: 可接受。这些都位于 HTTP 非 200 的错误分支中，ReadAll 仅用于构造更友好的错误消息，读取失败时使用空字符串作为 body 摘要即可。

### 2.5 其他 error 忽略

| 文件 | 行号 | 代码 | 风险 |
|------|------|------|------|
| `internal/store/records.go` | 213 | `n, _ := res.RowsAffected()` | 低）仅用于示例/调试 |
| `internal/targets/base.go` | 125,129,132 | `_ = w.WriteField(...)` | 低）multipart writer WriteField 极少失败 |
| `internal/targets/base.go` | 149 | `_ = w.Close()` | 低）关闭 multipart writer |
| `internal/source/source.go` | 621,632 | `_ = client.Delete(h, false)` | 低）清理操作，失败不影响主流程 |
| `cmd/relay/main.go` | 多处 | `_ = s.MarkStatus(...)` | 低）状态标记为 best-effort |
| `internal/web/server.go` | 179-180,236 | `fs.ReadFile` / `fs.Sub` 错误忽略 | 中）若 embed 的静态文件缺失，模板/ui 将静默损坏 |

**重点**: `internal/web/server.go:179-180` 的 `styleCSS, _ := fs.ReadFile(staticFS, "static/style.css")` 和 `appJS, _ := fs.ReadFile(staticFS, "static/app.js")` 忽略错误 -- 若构建时未正确 embed 静态文件，模板将以空内容渲染，页面没有任何样式和 JS，用户看到白屏但无错误提示。

---

## 3. HTTP Body Close

### 3.1 统计

搜索到 **25 处** `resp.Body.Close()` / `defer resp.Body.Close()` 调用，覆盖了所有 HTTP 请求路径。

### 3.2 逐个验证

| 文件 | 行号 | Close 方式 | 状态 |
|------|------|-----------|------|
| `internal/detail/api.go` | 232 | `defer resp.Body.Close()` | OK |
| `internal/detail/detail.go` | 315,340 | `defer resp.Body.Close()` | OK |
| `internal/source/source.go` | 391,476 | `defer resp.Body.Close()` | OK |
| `internal/qb/qb.go` | 146,271,286,395,431 | `defer resp.Body.Close()` | OK |
| `internal/qb/qb.go` | 215,461 | `resp.Body.Close()` (retry 路径) | OK |
| `internal/qb/autoinit.go` | 92,120 | `resp.Body.Close()` / `defer` | OK |
| `internal/qb/client.go` | 101,125,159,316 | `defer resp.Body.Close()` | OK |
| `internal/web/qb_proxy.go` | 136 | `defer resp.Body.Close()` | OK |
| `internal/targets/mteam.go` | 145 | `defer resp.Body.Close()` | OK |
| `internal/targets/nexusphp.go` | 136 | 错误路径手动 close | OK |
| `internal/web/setup.go` | 511,544,575 | `defer resp.Body.Close()` | OK |
| `internal/targets/base.go` | 164 | `readRespBody` 内部 `defer` | OK |

### 3.3 公共 close 工具函数

`internal/targets/base.go:163` 定义了 `readRespBody(resp *http.Response) (string, error)`，内部 `defer resp.Body.Close()`。以下调用方均通过此函数安全关闭:
- `nexusphp.go:139` (GetSections)
- `nexusphp.go:219` (UploadTorrent)
- `nexusphp_classic.go:167` (UploadTorrent)

`internal/qb/qb.go:270` 的 `expectOK` 和 `:285` 的 `parseAddResponse` 同样在内部 `defer resp.Body.Close()`。

### 3.4 评估

**通过** -- 所有 HTTP 响应体均已正确关闭，无泄漏。

---

## 4. 数据竞争分析

### 4.1 已使用锁的共享状态

| 位置 | 锁 | 保护对象 | 使用正确性 |
|------|-----|---------|-----------|
| `engine.go:30` | `cfgMu sync.RWMutex` | cfg, filter, retire | 正确 |
| `engine.go:41` | `statsMu sync.RWMutex` | stats | 正确 |
| `qb.go:79` | `slowMu sync.Mutex` | slowTracker | 正确 (在 client.go 中使用) |
| `source.go:225` | `qbMu sync.Mutex` | qbClient (懒初始化) | 基本正确 |
| `web/qb_proxy.go:29` | `mu sync.Mutex` | sid, expiry | 正确 |
| `web/auth.go:16` | `mu sync.Mutex` | sessions | 正确 |
| `web/auth.go:73` | `mu sync.Mutex` | rate limiter fails | 正确 |
| `web/server.go:42` | `tmplMu sync.RWMutex` | 模板缓存 | 正确 |
| `web/server.go:46` | `logBufMu sync.Mutex` | 日志缓冲区 | 正确 |
| `web/server.go:51` | `qbProxyMu sync.RWMutex` | qbProxy 引用 | 正确 |
| `store.go:73` | `mu sync.Mutex` | db 连接 | 正确 |

### 4.2 潜在数据竞争（中等）

**Web Server 与 Engine 共享 `*config.AppConfig`**

`cmd/relay/serve.go:67` 和 `:79` 中，Engine 和 Web Server 共享同一个 `cfg *config.AppConfig` 指针:

```go
eng, err := engine.New(cfg)          // Engine 持有 cfg 并通过 cfgMu.RLock() 读取
// ...
web.StartServer(cfg.Web.ListenAddr, eng, cfg)  // Server 也持有 cfg
```

Engine 读取 cfg 时正确使用了 `cfgMu.RLock()`（如 `cycle.go:15-17`, `monitor.go:126-129`），但 **Web Server 的 setup 处理器直接修改 `s.cfg` 字段而不持有任何锁**:

| 文件 | 行号 | 修改内容 |
|------|------|---------|
| `internal/web/setup.go` | 192-196 | `s.cfg.QB.Host/Port/Username/Password/UseSSL` |
| `internal/web/setup.go` | 269 | `s.cfg.Sources = [...]` |
| `internal/web/setup.go` | 349 | `s.cfg.Targets = [...]` |
| `internal/web/setup.go` | 397,413 | `s.cfg.Web.Password` / `s.cfg.QB.Password` |

**实际风险**: 设置向导通常在引擎启动前完成，且 `handleSetupComplete` 会保存到磁盘后标记 `initialized`。但若用户在设置完成后热更新配置（或未来增加运行时修改配置的功能），Engine 读取配置的 goroutine 与 HTTP handler 写入配置的 goroutine 将形成数据竞争。

**建议**: 
1. 在 setup handler 修改 `s.cfg` 前通过 Engine 接口获取一个锁（如 `e.cfgMu`）
2. 或使用 `atomic.Pointer` / 不可变快照模式传递配置

### 4.3 其他注意事项

- `source.go:543` 使用 `qbMu sync.Mutex` 保护 `qbClient` 的懒初始化，是正确的一把大锁模式。可考虑改用 `sync.Once` 更惯用。
- `auth.go` 的 `sessionStore` 和 `rateLimiter` 各自持有独立的 `sync.Mutex`，使用正确。

---

## 5. 配置文件路径

### 5.1 默认路径

| 用途 | 默认值 | 定义位置 |
|------|--------|---------|
| 主配置 | `config/relay.yaml` (fallback `config/relay.json`) | `relay_config.go:230` |
| 数据库 | `data/relay.db` | `relay_config.go:147` / `store.go:80` |
| 种子下载目录 | `data/torrents` | `relay_config.go:149` |
| Web 监听地址 | `:9020` | `relay_config.go:184` |

### 5.2 查找链

1. CLI `--config` flag (serve.go:31)
2. 若未指定，依次尝试 `config/relay.yaml`、`config/relay.json` (relay_config.go:230)
3. 若都不存在，启动设置向导模式 (serve.go:44)

### 5.3 评估

**通过** -- 路径合理，查找链完整，错误提示清晰。所有路径均为相对于工作目录的相对路径，符合服务类程序惯例。

---

## 6. 未使用的 Import

**结果: 通过**

`go vet` 未报告任何未使用的 import。所有导入的包均有实际引用。

---

## 7. TODO/FIXME 遗留

### 找到 1 处 TODO

| 文件 | 行号 | 内容 |
|------|------|------|
| `internal/engine/engine.go` | 300 | `// TODO: implement record log` |

关联代码:
```go
Records: []web.RecordEntry{}, // TODO: implement record log
```

种子详情 API 的 Records 字段硬编码为空切片，尚未实现操作日志/时间线功能。

### 未找到 FIXME / HACK / XXX 标记

---

## 8. 附加入发现

### 8.1 未使用的结构体字段（信息）

`internal/qb/qb.go:79` 声明了 `slowMu sync.Mutex` 和 `slowTracker map[string]*slowEntry` 字段，实际使用在 `internal/qb/client.go` 的 `IsSlow` 和 `ResetSlow` 方法中。这是正常的同包跨文件使用，不是问题。

### 8.2 故意空操作（编译器提示抑制）

| 文件 | 行号 | 代码 | 说明 |
|------|------|------|------|
| `internal/descr/descr.go` | 375 | `_ = i` | 循环变量在 return 前未使用，抑制编译错误 |
| `internal/descr/descr.go` | 579 | `_ = title` | 函数参数未来可能使用 |
| `internal/engine/engine.go` | 449-450 | `var _ = targets.Upload` | 确保包引用，防止 `go mod tidy` 移除 |
| `internal/web/qb_proxy.go` | 171 | `var _ = httputil.ReverseProxy{}` | 同上 |
| `cmd/relay/main.go` | 783,824 | `_ = out` / `_ = cookie` | 预留变量 |

### 8.3 静态文件 embed 错误处理（重复强调）

`internal/web/server.go:179-180`:
```go
styleCSS, _ := fs.ReadFile(staticFS, "static/style.css")
appJS, _ := fs.ReadFile(staticFS, "static/app.js")
```

若 embed 失败（如构建标签错误、go:embed 路径写错），模板将以空字符串渲染样式和 JS，前端完全无法使用且无错误日志。

---

## 总结

| 检查项 | 状态 | 严重程度 |
|--------|------|---------|
| go vet | 通过 | -- |
| Error 处理 | 基本通过，3 个关注点 | 低 |
| HTTP Body Close | 通过 | -- |
| 数据竞争 | 1 个潜在问题 | 中 |
| 配置文件路径 | 通过 | -- |
| 未使用 Import | 通过 | -- |
| TODO/FIXME | 1 个 TODO | 低 |

### 优先修复建议

1. **数据竞争** (中等): Web Server setup handler 修改 `s.cfg` 时增加锁保护，或与 Engine 的 `cfgMu` 协调
2. **url.Parse 错误忽略** (中等): `qb.go:124` 的 `SID()` 方法增加 nil check
3. **embed 文件读取** (低): `server.go:179-180` 的 `fs.ReadFile` 错误应至少在启动时日志告警
4. **TODO 实现**: `engine.go:300` 的 record log 功能待补齐
