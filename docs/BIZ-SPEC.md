# AutoSeedRelay 业务定稿 v1（重构立项依据）

> 2025 与用户逐项对齐确认。本文是业务语义的**唯一权威来源**，代码重构以此为准。
> 现状代码与本文冲突处以本文为准；未覆盖细节见各 docs 参考文档。
> 实现修订（M3）：实现与本文存在差异且实现合理处，已就地修正并标「实现修订」注记，汇总见 §11。

## 1. 业务目标

自动把「源站」新种子搬运到多个「目标 PT 站」：蹲 RSS → 下载 → 清洗适配 → 发布；目标站已存在则自动交叉辅种；做种达标后自动撤种。全程无人值守，异常可自愈或可告警。

**成功标准**：种子从命中到目标站上线/辅种全自动；零重复发布；活动可追溯；多人/多实例不撞车（原子抢发）；撤种按策略且原因入库。

**自动化边界（仍需人）**：新站点接入的枚举探测与人工核对；通知渠道配置；手动重发。

## 2. 站点拓扑（用户实际配置）

| 角色 | 站点 | 适配器 | 备注 |
|---|---|---|---|
| 源站 | SOURCE（内部代号） | — | 单源站（数据模型保留多源站能力） |
| 目标 | T1（代号） | NexusPHP API | 二开，基本标准 NP |
| 目标 | T2（代号） | M-Team 专用 | 自研架构 |
| 目标 | T3（代号） | NexusPHP classic | 魔改严重，重点测试对象 |
| 测试 | DEV（内部代号） | 同 NP API | 发布前测试站；**标准 NP 适配器的参考实现**；DEV API 已知有问题，重构时排查并汇报 |

> 注：站点真实域名/凭据仅存在于部署配置与数据库中，本仓库文档一律用代号，不暴露实际拓扑。

三套目标站适配器**全部保留**，一个都不能砍。

**NP 版本分界（用户确认）**：两个 NP 目标站——≥1.9 走 API（T1），<1.9 走 cookie/classic（T3，魔改严重）。标准 NP 行为以 DEV 测试站为准。

## 3. 核心流程（定稿）

1. **轮询**：每 poll_interval 秒拉源站 RSS（启动立即一轮）。
2. **筛选**：促销白名单 + 关键词 + 大小范围，三者 AND；全空=全收；缺失字段不参与对应条件（空值语义对称化）。
3. **去重**：按 `(source_site, info_hash)` 复合键；Poller 首见即写 `seen_hashes` 永久 tombstone（实现修订：即使 seeds 行被删除或旧备份恢复，也不会重新入队，见 §11）。
4. **下载**：RSS 直下或指定 qB 直拉（按分派策略选 qB，见 §7）。
5. **补详情**：详情页/API 抓文件列表、标签、MediaInfo、IMDb。
6. **清洗适配**：改 tracker/private/source；标题规范化（titler 结构化解析填维度/分类，标题保留源站规范后版本，修正 Python 怪癖）；简介清洗重组；标签按目标站映射。
7. **发布/辅种**：对每个启用目标站**独立处理、互不影响**；上传命中"已存在" → **自动交叉辅种**（下载目标站种子，指向已有数据目录 skip_checking 回挂 qB）。
8. **监控**：遍历所有启用 qB：在线状态/做种统计/磁盘/低速率/撤种判定。
9. **撤种**：条件满足 → 停种删除 → 标 retired 记录原因；**撤后永久去重，面板可手动重发**。

**重试**：失败自动重试（次数 = strategy.retry_max 默认 3，指数退避 60s/300s/900s），仍失败进失败队列（status=failed）+ critical 告警。**目标级重试（实现修订）**：部分目标成功、部分失败（PartialFailure）时仅重跑未完成目标；重试耗尽后保留成功目标（seed 保持 seeding）、仅对失败目标 critical 告警。面板可手动重发（`POST /seeds/{id}/resend`，`full=true` 全量重跑）；cookie 过期识别并告警。

## 4. 实体模型

- **源站 Source**：name/role/rss_url/凭据(cookie/passkey/api_token)。
- **目标站 Target**：name/type(nexusphp|nexusphp_classic|mteam)/base_url/announce/凭据/分类与标签映射/fallback。
- **qB 实例 QBInstance**（多台）：name/host/port/user/pass/enabled/priority；分派策略见 §7。
- **种子 Seed**：id/(source_site,info_hash)/title/size/category/promotion/source_id/qb_name/status/error/时间戳。
- **发布记录 RelayRecord**：seed_id/target_site/role(publisher|seeder)/status/target_id/target_hash/retired_at/reason/seeders/leechers/统计。
- **通知实例 NotifierInstance**：id/type(provider)/name/凭据/启用；事件路由见 §8。
- **活动日志 ActivityLog**：seed_id/action/detail/时间；滚动保留（默认 30 天，可配）+ 关键记录全保留。

## 5. 状态机（统一一套，两级）

**种子级（seed.status，9 态）**：
`discovered → downloading → downloaded → processing → seeding → retired | failed | skipped`
- discovered：初始态（Poller 入库，尚未处理）——**实现修订**：原「pending」改名为「discovered」
- downloading→downloaded：下载完成（先下载后标 downloaded）
- processing：清洗适配中（Relay 开始即置，含详情抓取阶段）
- seeding：至少一个目标站已发布/辅种
- retired：撤种；failed：重试耗尽或不可重试错误；skipped：人工跳过/筛选跳过
- retry：**内部态**（重试队列等待中，失败后引擎置位，重启后从该态重建队列）——**实现修订**：新增

**记录级（relay_record.status，每目标站一条，8 态白名单）**：
实际流转 `pending → published | cross_seeding | failed → retired`
- pending：记录创建/未处理；published：发布成功；cross_seeding：撞车降级辅种
- failed：该目标失败（目标级重试时仅重跑此类）；retired：撤种
- **实现修订**：白名单含 `uploading`、`skipped_existing`，但当前 pipeline 直接 pending→published/cross_seeding（未单独写 uploading），撞车走 cross_seeding（不再用 skipped_existing）；白名单 8 态为 pending/uploading/published/cross_seeding/seeding/failed/retired/skipped_existing

## 6. 业务规则定稿

**筛选**（filter）：
- 促销别名：free/2x_free/2x/50%/30%/neutral + 别名表；**精确层级匹配**（"free"不再误命中"2x_free"）；未配置=全收；未知促销名→按不匹配处理并记日志。
- 关键词：标题含任一即命中（大小写不敏感）；未配置=全收。
- 大小：0=该侧不限；RSS 缺失 size 时**不参与**大小过滤（不再被 MinSize 误拒）。

**标题**：修正 Python 怪癖（编码/声道 token 不再被吞）；titler 结构化解析结果用于填维度/分类字段，上传标题=源站标题规范化（双轨）。

**标签映射**：归属型标签跨站跳过、客观属性保留（TAG-MAPPING 现有规则）；classic 站 tags/维度并入 descr 段。

**分类/维度**：分类回退链保留（数字直通→API 枚举→别名→默认→fallback）；**维度 ID 支持 per-site 枚举覆盖**（GetSections 解析 *_list 注入，不再硬编码）。

**去重键**：`(source_site, info_hash)` 复合唯一；guid 仅作辅助，空 guid 不再 sha1("") 碰撞。

**抢发/辅种**：多实例原子抢发（INSERT ON CONFLICT DO NOTHING 抢 publisher），撞车降级 seeder；目标站返回已存在 → 交叉辅种。

**撤种**（默认值，均可配）：
- 条件：做种人数 ≥ 10；做种时长 > 60 分钟；分享率 **默认关闭**
- 组合模式：**可配置** AND（全部满足）/ OR（任一满足）
- 动作：停种 → 删除（delete_files 可配）→ retired + 原因入库
- 磁盘紧急/低速率：独立于撤种策略的紧急中止路径（**实现修订**：阈值入 strategy 单行——`disk_low_gb`=50 / `disk_critical_gb`=20 告警，`low_speed_kbps`=100 持续 `low_speed_duration_sec`=600 触发 `low_speed_action`=abort 中止，见 §11）

**图片转存（可选功能）**：可配置图床 URL + token；开关「封面转存」；默认关。

## 7. 多 qB 实例（新增需求）

- 主程序管理**多台 qB**，qB 为一级配置实体（增删改 + 连接测试 + 状态展示）。
- **分派策略**（全局可配，单选）：手动优先级（按 priority 降序）/ 剩余磁盘最多 / 任务数最少（least_jobs）/ 轮询均衡（round_robin）。
- 下载/交叉做种均走分派策略；交叉做种**优先选种子当前所在 qB**。
- 监控循环遍历全部启用 qB；单台断连不影响其他。

## 8. 通知系统（新增需求）

**三层架构**：
- **Provider 类型**（内置）：webhook（企业微信/钉钉/飞书/自建）、telegram、smtp 邮件、ntfy/gotify、serverchan/pushplus（预留）。
- **实例 Instance**：同一 provider 可配**多份**（如两个 TG bot）；每份独立凭据、独立开关。
- **事件分层 Tier**：critical（发布失败重试耗尽、qB 全断、磁盘 critical、cookie 过期）；warning（磁盘 low、单台 qB 断连）；info（发布成功、降级辅种、自动撤种、每日汇总）。
- **路由矩阵**：实例 × Tier 勾选；一层可绑多个实例；**同实例 × tier × 事件 10 分钟聚合**防刷屏（critical 不聚合；聚合键含事件标签，语义不同的事件分开合并）。
- 全部默认关，不配置零负担。

## 9. 仪表盘（提案，见对话）

单页五区：状态条（qB×N/源站/磁盘/轮询倒计时）→ 统计卡（累计/今日发布辅种、当前做种、7 天成功率）→ 进行中任务 → 最近事件流 → 7 天趋势。详见对话中提案，用户确认后并入。

## 10. 技术栈与范围

- 后端：Go + **Gin**；存储 **SQLite**（modernc 纯 Go，手写 SQL 仓储 + 嵌入式迁移）；PostgreSQL/sqlc/goose 为原始预留未落地（实现修订见 ARCHITECTURE-v4 §17）。
- 前端：Vue 3 + Vite + TS + Element Plus + Pinia，独立工程。
- 保留：三套目标站适配器、qB 客户端逻辑、bencode（加固）、descr 清洗、titler（修正）。
- 砍掉：CLI 6 个子命令（只留 serve）；旧 config.go 死代码路径；relay_jobs 旧表；python_compare 怪癖固化测试。
- 修复清单：见《项目分析报告》P0/P1 全部 + 业务缺陷 23 条（docs/BIZ-SPEC 附录来源）。
- **备份导出（Web）**：一键下载 zip（数据库 + 业务配置 + 部署配置脱敏版）；导入恢复功能随后。

## 11. 实现修订注记（M3）

实现与本文存在差异且实现合理处，就地修正并汇总于此：

| 主题 | 本文原文 | 实现修订 |
|---|---|---|
| 种子初始态 | pending | discovered（内部态） |
| 种子内部态 | — | retry（重试队列等待中，重启可重建） |
| 记录级流转 | pending → uploading → published…skipped_existing | 实际 pending → published/cross_seeding/failed → retired；uploading/skipped_existing 保留白名单但当前不写 |
| 重试语义 | 全种子级重试 | PartialFailure 目标级重试（仅重跑未完成目标，耗尽保留成功目标） |
| 去重 | 仅 (source_site, info_hash) 复合键 | 新增 seen_hashes 永久 tombstone（删除/旧备份也不重入队） |
| 通知聚合 | 同实例同事件 | 同实例 × tier × 事件（语义不同事件分开合并） |
| 磁盘/低速率 | 描述性 | 阈值入 strategy 单行：disk_low_gb=50 / disk_critical_gb=20 / low_speed_kbps=100 / low_speed_duration_sec=600 / low_speed_action=abort |
| 手动重发 | 面板可手动重试 | `POST /seeds/{id}/resend`（full=true 全量重跑，删除全部记录+副本） |
