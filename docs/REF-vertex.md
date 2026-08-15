# Vertex 参考提取

> 源码已浅克隆至 `vertex-ref/`（GitHub 直连 schannel 报错，改用 `git -c http.sslBackend=openssl` 成功）。以下路径均相对 `vertex-ref/`。

## 1. 项目概况
- 技术栈：后端 Node.js + Express；前端 Vue3 + Ant Design Vue + ECharts + xterm；存储 better-sqlite3（指标）+ JSON 文件（配置）+ Redis/redlock（session/锁）；puppeteer+stealth（站点自动化）；express-http-proxy（反代）；node-cron（定时）。
- 定位：PT「追剧刷流一体化」工具，功能远超辅种——RSS 订阅、豆瓣追剧、多下载器（qB/Deluge/Transmission）、40+ PT 站适配器、删种规则引擎、通知、SSH 服务器监控。
- 状态：作者声明「不新增功能，仅修问题」（README.md）。

## 2. qB 接入与免鉴权拉起
**接入方式（直连 qB Web API v2，非内嵌）**，`app/libs/client/qb.js`：
- 登录走 `/api/v2/auth/login`，从 `set-cookie` 头取 `SID=xxx`；兼容 qB 5.2.0+ 的 204 空响应（旧版 200 "Ok."/"Fails."）（qb.js:6-33）。
- 版本探测+缓存 `getCachedApiVersion()`（qb.js:50），据版本做兼容分支：`paused/stopped`、`resume/start`（>2.9.3 用 stopped/start）。
- 数据同步 `/api/v2/sync/maindata` + 字段白名单映射 `torrentFilter`/`serverFilter`（qb.js:341），只取需要的字段。
- 能力面：加种（URL/torrent 文件）、tag、删除、限速、改文件优先级、分类、reannounce、读日志。

**免鉴权拉起（反向代理注入 SID）**，`app/routes/router.js:67`：
- 路由 `/proxy/client/:client` 用 express-http-proxy 反代到 qB 的 `clientUrl`，`proxyReqOptDecorator` 把内存里的 `global.runningClient[id].cookie`（SID）注入请求头，浏览器免登录直接打开 WebUI。
- 前端一键打开：`webui/src/pages/base/Downloader.vue:437` `window.open('/proxy/client/${id}/')`。
- 同机制复用给站点：`/proxy/site/:site`（router.js:90）注入站点 cookie + 重写 HTML 绝对路径（src/href）+ 设 Referer + 删 x-forwarded-for。

**认证与安全边界**：
- SID 只存内存（`global.runningClient[id].cookie`），启动时重新 login；`lastCookie` 超 3000s 自动重登（`app/common/Client.js:264`）。
- 面板自身鉴权 = session（Redis store，30 天），`checkAuth` 放行 `/api/user/login`、assets、`/api/openapi/*`（router.js:26）。
- 风险：qB 密码、站点 cookie 明文存 JSON（`data/client/{id}.json`）；crypto-js 仅用于 cookiecloud 解密，未加密存储。

## 3. 通知设计
- 渠道（`app/libs/push/`）：`wechat`、`slack`、`telegram`、`ntfy`、`webhook`，注册表见 `app/common/Push.js:9`。
- 组织：**事件 = 方法名**。每个 provider 实现同名方法（`addTorrent`/`deleteTorrent`/`torrentFinish`/`spaceAlarm`/`clientLoginError`…），`Push` 包装类统一分发；新增事件只需给 provider 加方法。
- 路由/过滤：每个渠道配 `pushType[]`（订阅事件白名单），`doRequest` 先查 `pushType.indexOf(type)`，未订阅直接 return。
- 聚合防刷屏（`app/common/Push.js:46`）：`errorCount` + `maxErrorCount`（默认 100）+ `clearCountCron`（默认每小时清零）；`*Error`/`*Failed` 类事件累加计数，周期内超阈值即跳过。
- 双通道：每个下载器有 `notify`（事件）+ `monitor`（监控）两路；monitor 的 Telegram 用 `editMessageText` 复用同一 `message_id` 刷新状态，避免刷屏（`app/libs/push/telegram.js:31`）。
- Webhook 出站：`x-vertex-token` 头 + 点分事件名（`rss.torrent.add`/`client.torrent.delete`），见 `app/libs/push/webhook.js`。
- Webhook 入站：`/openapi/:apiKey/{plex,emby,jellyfin,wechat,slack}`，apiKey 放 URL 路径、排除在 session 鉴权外（router.js:285）。

## 4. 多客户端/多站点管理
- 数据模型：**配置 = JSON 文件**（`data/{client,site,server,push,rss,rule/*}`，一实体一文件），**指标/时序 = SQLite**（`torrent_flow`/`tracker_flow`/`sites`）。`app/libs/util.js` 的 `listClient/listPush/listSite/listServer` 即读目录 + import JSON。
- 运行模型：每个启用实体一个内存实例（`global.runningClient[id] = new Client()`），实例内挂各自 cron（maindata 轮询、空间告警、自动删种、tracker 同步）；add/modify/delete = destroy + 重建（热重载，`app/model/ClientMod.js`）。
- 多客户端：`app/common/Client.js:11` 适配器注册表 `{qBittorrent, deluge, Transmission}`；字段含 alias/type/url/user/pass/notify/monitor/cron/autoDelete 规则/空间阈值/`sameServerClients`（同机分组）。
- 多站点：`app/libs/site/index.js` 启动扫目录 `new Site()` 自动注册 `getInfo/searchTorrent/getDownloadLink/siteUrl` 到全局 map（插件化，40+ 适配器）。
- 多服务器：`ssh2` 远程监控（CPU/内存/磁盘/vnstat）+ WebSocket web shell（xterm，`app/model/ServerMod.js:142`）。
- UI：表格 + 行内表单（Ant Design Vue）；`used` 引用计数（被 RSS/豆瓣/watch 引用则禁止禁用/删除）、克隆配置、启用开关（`webui/src/pages/base/Downloader.vue`）。

## 5. 可借鉴点清单
1. 下载器 adapter 注册表（type→实现）→ 我们多 qB 抽象成 `DownloaderProvider` 接口，未来扩展 Deluge/Transmission。
2. 每客户端独立轮询任务 + 热重载 → Go 里每客户端一个 goroutine + context.CancelFunc，配置变更即重启。
3. maindata 字段白名单（torrentFilter/serverFilter）→ 只取需要的 qB 字段，降低大实例带宽/内存。
4. qB API 版本探测 + 缓存 + 兼容分支（204/paused/stopped）→ 处理 qB 4.x/5.x 差异，避免接口报错。
5. 通知「事件=方法名 + pushType 白名单 + 错误聚合限流」→ 我们通知系统核心模型可直接套用。
6. monitor 通道用「编辑同一条消息」（telegram editMessageText）→ 长时间运行状态更新不刷屏。
7. 反代注入 SID 免鉴权拉起 WebUI + rejectUnauthorized=false + HTML 相对路径重写 → 直接照搬到 Gin（httputil.ReverseProxy）。
8. 配置 JSON 文件 + 指标 SQLite 分离 → 配置易备份/迁移/版本化，保留「配置与指标分离」思想。
9. OpenAPI 入站 webhook 用 apiKey 放路径、排除 session 鉴权 → 通知入站统一用签名/密钥。
10. 引用计数 `used` + 克隆配置 → 删除安全性与运维效率。
11. 全局网络代理 + 按域名路由（`proxy.json` 的 `proxy`/`domains`）→ 多网络环境友好。

## 6. 不建议照搬的点
1. 明文存 qB 密码/站点 cookie（JSON 文件）→ 我们应加密存储或用密钥管理（环境变量/Secret）。
2. Node 全局可变状态 `global.runningClient`（大对象 + 隐式依赖）→ Go 用 struct + sync.Map + 显式依赖注入。
3. better-sqlite3 同步阻塞 + 巨型 router.js（300+ 行）+ 每实体手写 CRUD controller/model → 我们 Gin 分层 + 统一 CRUD。
4. Ant Design Vue + less 主题编译（antd-theme-generator，暗黑主题靠静态 less 变量表）→ 我们用 Element Plus，主题用 CSS 变量。
5. Redis 强依赖（session store + 缓存 + 锁）→ 单机部署可省，session 可落库或用 JWT。
