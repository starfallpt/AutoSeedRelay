# AutoSeedRelay Go Code Security Review

**审查日期**: 2026-08-12
**审查范围**: Go 源码全量（`cmd/`、`internal/`）
**审查方法**: 静态代码审查，覆盖凭据泄漏、日志泄漏、SQL 注入、路径遍历、SSRF、环境变量校验

---

## 发现总览

| 等级 | 数量 | 说明 |
|------|------|------|
| 严重 | 1 | 可远程利用或会导致凭据暴露 |
| 警告 | 5 | 存在风险但利用条件受限 |
| 建议 | 5 | 加固建议，不影响核心安全 |

---

## 一、严重

### [严重-1] QB 密码通过 /api/config 接口暴露

**文件**: `internal\config\relay_config.go`
**行号**: 52-55

**问题**: `QBConfig.Password` 字段缺少 `json:"-"` 标签，而 `WebConfig.Password`（同文件第 137 行）已正确添加了 `json:"-"` 标签。当 Web UI 调用 `GET /api/config` 时，引擎通过 `Engine.GetConfig()`（`internal\engine\engine.go:342-343`）返回 `*AppConfig`，JSON 序列化时会将 qBittorrent 密码明文输出给前端。

```go
// relay_config.go:50-55
type QBConfig struct {
    Host     string `yaml:"host" json:"host"`
    Port     int    `yaml:"port" json:"port"`
    Username string `yaml:"username" json:"username"`
    Password string `yaml:"password" json:"password"`  // <-- 缺少 json:"-"
    UseSSL   bool   `yaml:"use_ssl" json:"use_ssl"`
}

// relay_config.go:134-138（Web 密码正确处理）
type WebConfig struct {
    ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
    Password   string `yaml:"password" json:"-"`  // <-- 正确
}
```

**影响**: 任何通过 Web 面板认证的用户都可获取 qBittorrent 的明文密码。

**建议**: 将 `QBConfig.Password` 的 json 标签改为 `json:"-"`。

---

## 二、警告

### [警告-1] SaveAppConfig 保存时 SiteProfile 凭据未脱敏

**文件**: `internal\config\config.go` 行 107-109; `internal\config\relay_config.go` 行 382-399

**问题**: `SaveAppConfig` 函数在第 393 行对 `QB.Password` 做了脱敏（`saved.QB.Password = "***"`），但 `SiteProfile` 中的 `APIToken`（站点 API 令牌）、`MTeamAuth`（M-Team x-api-key）、`Cookie`（登录 Cookie）未做脱敏处理。由于 `SiteProfile` 的这些字段没有 `json:"-"` 标签，一旦通过 Web UI 保存配置，这些凭据将以明文写入 `config/relay.yaml`。

此外，`GET /api/config` 也会返回这些 SiteProfile 凭据字段（`BaseURL`、`RSSURL` 中的 passkey 等）。

```go
// relay_config.go:381-399
func SaveAppConfig(cfg *AppConfig, path string) error {
    ...
    saved := *cfg
    saved.QB.Password = "***"  // 仅掩码了 QB 密码
    // SiteProfile 的 APIToken / MTeamAuth / Cookie 未掩码!
    ...
}
```

**建议**:
1. `SiteProfile.APIToken`、`SiteProfile.MTeamAuth`、`SiteProfile.Cookie` 添加 `json:"-"` 标签
2. `SaveAppConfig` 中对所有 SiteProfile 敏感字段做脱敏处理

### [警告-2] HTTP 下载 URL 含 passkey 经错误消息输出

**文件**: `internal\source\source.go` 行 367, 行 502-514

**问题**: `torrentURLs` 方法（第 367 行）将 passkey 拼接进下载 URL：
```go
urls = append(urls, fmt.Sprintf("%s/download.php?id=%s&passkey=%s", site, item.ID, c.Passkey))
```
当下载失败时，`describeFail` 函数（第 502-514 行）将完整 URL（含 passkey）作为错误信息返回：
```go
return fmt.Sprintf("[%s] download %s: HTTP %d ct=%q server=%q (%s body=%dB, %q)",
    backend, u, status, ctype, server, tag, len(body), snippet)
```
这些错误信息可通过日志（`log.Printf` / `slog.Error`）和 store 的 error 字段（流向 Web UI）两种路径泄漏 passkey。

**建议**: `describeFail` 中对 URL 做脱敏处理，移除或替换 query 参数中的 `passkey=xxx` 部分。

### [警告-3] RSS enclosure URL 直接用于 HTTP 下载（SSRF 风险）

**文件**: `internal\source\source.go` 行 370-371, 行 444

**问题**: `torrentURLs` 方法将 RSS feed 中的 `EnclosureURL` 直接加入下载候选列表：
```go
if item.EnclosureURL != "" {
    urls = append(urls, item.EnclosureURL)
}
```
`rawGet` 方法直接向此 URL 发起 HTTP GET 请求。如果源站 RSS 通过 HTTP（非 HTTPS）传输，或源站被入侵，攻击者可注入恶意的 enclosure URL，导致：
- 服务端 SSRF（扫描内网、访问内部服务）
- 下载恶意的 .torrent 文件

此外，`source.go` 第 285 行配置了环境变量代理 `http.ProxyFromEnvironment`，SSRF 可能绕过代理直连内网。

**建议**:
1. 验证 enclosure URL 的 host 与配置的 `base_url` host 一致
2. 限制下载 URL 的协议为 https（如果源站支持）
3. 对下载 URL 的 host 做白名单校验

### [警告-4] Web 面板默认密码为弱口令

**文件**: `internal\config\relay_config.go` 行 185

**问题**: Web 管理面板默认密码为 `"admin"`，若用户未通过环境变量 `AUTOSEED_WEB_PASSWORD` 覆盖，则使用此弱口令。Web 面板监听在 `:9020`（行 184），若无防火墙限制，可被外部访问。

```go
Web: WebConfig{
    ListenAddr: ":9020",
    Password:   "admin",        // <-- 弱密码
},
```

**建议**:
1. 启动时若密码为默认值，打印警告日志提示用户修改
2. 或在未检测到 `AUTOSEED_WEB_PASSWORD` 环境变量且无自定义配置时，拒绝启动并给出提示

### [警告-5] QB 登录失败仅警告，无凭据验证启动检查

**文件**: `internal\engine\engine.go` 行 142-147

**问题**: 引擎启动时 qB 登录失败仅记录 Warning 日志，引擎仍继续运行。后续 RSS 轮询、种子下载、上传等功能可能静默失败，掩盖配置错误。

```go
if err := e.qb.Login(); err != nil {
    slog.Warn("engine: qb login failed, will retry", "error", err)  // 仅警告，继续
}
```

**建议**: 考虑在启动时至少验证一次 qB 连接，失败时给出明确错误并要求确认。或增加启动健康检查阶段。

---

## 三、建议

### [建议-1] CLI 参数可直接传入 cookie/token

**文件**: `cmd\relay\main.go` 行 798-799

**问题**: `probe` 子命令的 `--cookie` 和 `--token` 标志可直接从命令行传入敏感凭据。命令行参数在进程列表（`ps aux`）中可被其他用户看到。

```go
cmd.Flags().String("cookie", os.Getenv("SITE_COOKIE"), "登录 cookie(...)")
cmd.Flags().String("token", os.Getenv("SITE_TOKEN"), "Bearer token(...)")
```

**建议**: 在文档中强调优先使用环境变量 `SITE_COOKIE` / `SITE_TOKEN`，而非 `--cookie` / `--token` 命令行参数。或考虑移除 CLI 明文凭据参数，仅支持环境变量。

### [建议-2] SQL 列名拼接使用内部常量，但仍建议 review

**文件**: `internal\store\store.go` 行 143-155, `internal\store\seeds.go` 行 187-213

**问题**: `updateRow` 和 `UpdateSeedStatus` 方法将列名拼接到 SQL 语句中（`k+"=?"`），但列名来自内部白名单（`updatableSet` / `allowed` map），不接受外部输入，风险极低。

所有 SQL 值均使用 `?` 占位符传递，无直接拼接用户输入的情况。**SQL 注入风险：无**。

### [建议-3] 种子下载路径依赖 info_hash，安全性好但需关注临时文件

**文件**: `internal\pipeline\pipeline.go` 行 126

**问题**: 清洗后种子文件路径为 `filepath.Join(outDir, fmt.Sprintf("clean_%s.torrent", truncateStr(parsed.InfoHash, 12)))`，基于 info_hash 构造，无法利用路径遍历。`outDir` 来自配置 `cfg.OutDir`（默认 `data/out`），用户无法通过输入控制。

**路径遍历风险：无**（文件路径均通过 `filepath.Join` 从配置或 info_hash 构造）。

**建议**: 确保 `bencode.LoadTorrent(path)` 和 `os.ReadFile(path)` 的所有调用方 path 参数均不是直接的用户输入（当前实现正确）。

### [建议-4] session cookie 缺少 Secure 属性

**文件**: `internal\web\auth.go` 行 203-210

**问题**: Web 面板的 session cookie 设置了 `HttpOnly` 和 `SameSite=Strict`，但未设置 `Secure` 属性。如果面板通过 HTTP 暴露（默认行为），cookie 可被网络中间人截获。

```go
http.SetCookie(w, &http.Cookie{
    Name:     "session",
    Value:    token,
    Path:     "/",
    HttpOnly: true,
    SameSite: http.SameSiteStrictMode,
    MaxAge:   86400,
    // Secure: true,  // <-- 缺失
})
```

**建议**: 若面板配置为 HTTPS，应设置 `Secure: true`。或增加配置选项控制。

### [建议-5] 环境变量 SITE_TOKEN/SITE_COOKIE 无格式校验

**文件**: `cmd\relay\main.go` 行 797-799, `internal\config\config.go` 行 731-733

**问题**: `probe` 命令和环境变量 `AUTOSEED_<SITE>_TOKEN` / `AUTOSEED_<SITE>_COOKIE` 的值直接使用，不做格式校验。虽然不会造成漏洞，但空值或格式错误可能导致功能静默失败。

**建议**: 在配置加载阶段，对关键令牌格式做基本校验（如 M-Team auth 格式、cookie 至少包含 `=` 分隔符等），便于早期发现问题。

---

## 四、检查项汇总

| 检查项 | 状态 | 关键文件/行号 |
|--------|------|--------------|
| 硬编码凭据 | 发现默认密码（admin/adminadmin） | `relay_config.go:155,185` |
| API 凭据暴露 | **严重**: QB 密码经 /api/config 暴露 | `relay_config.go:54` |
| 日志泄漏凭据 | **警告**: 下载错误信息含 passkey URL | `source.go:367,513-514` |
| 配置文件凭据泄漏 | **警告**: SaveAppConfig 未对 SiteProfile 脱敏 | `config.go:107-109`, `relay_config.go:393` |
| SQL 注入 | **通过**: 全部使用 ? 参数化查询 | store/*.go |
| 路径遍历 | **通过**: 路径从内部值构造,非用户输入 | `pipeline.go:126`, `bencode.go:201` |
| SSRF | **警告**: RSS enclosure URL 未验证主机 | `source.go:370-371,444` |
| 环境变量默认值 | **警告**: Web 面板默认密码 admin | `relay_config.go:185` |
| 启动凭据校验 | **警告**: QB 登录失败不阻止启动 | `engine.go:142-147` |
| Session 安全 | 缺少 Secure 属性 | `auth.go:203-210` |

---

## 五、修复优先级

1. **立即修复**: [严重-1] QBConfig.Password 添加 `json:"-"` 标签
2. **高优先级**: [警告-1] SiteProfile 凭据字段脱敏 + `json:"-"` 标签
3. **高优先级**: [警告-2] 错误消息中对 URL 做 passkey 脱敏
4. **中优先级**: [警告-3] RSS enclosure URL 主机白名单校验
5. **中优先级**: [警告-4] Web 面板默认密码启动时警告/拒绝
6. **低优先级**: [建议-1] ~ [建议-5] 逐步加固

---

*审查工具: 人工代码审查 + ripgrep 模式搜索*
