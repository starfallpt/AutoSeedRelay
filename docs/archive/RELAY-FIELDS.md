# 转种所需数据

> 一条种子从源站到目标站，需要三组数据：**源站提取字段**（详情页/RSS/种子文件）、
> **目标站必填/可选字段**（三种适配器差异）、**站点差异速查**（各站 ID/表单槽位差异）。
> 对应 Go 实现：`internal/detail/`、`internal/source/`、`internal/descr/`、
> `internal/targets/`。

---

## 1. 源站提取

源站（NexusPHP 系）从 RSS 与详情页补充提取的字段。`RSS → 详情页` 逐级补全：
RSS 自带 `title/descr/category/size/guid`，详情页补 `small_descr/tags/imdb`，
`viewfilelist.php` 补文件列表。

| 字段 | 来源 | 说明 |
|------|------|------|
| 种子标题 | 详情页 `<title>` 标签 | 不含中文前缀——中文前缀剥离到副标题（M-Team 规则） |
| 副标题 | 详情页 rowhead `"副标题"` | `ParseSmallDescr`，取同行的 `rowfollow` 单元格 |
| 标签 | 详情页 rowhead `"标签"` | `ParseTags`，标签行内 `<span>` 文本列表 |
| MediaInfo | `<pre>` 标签或 mediainfo 折叠块 | 完整 General/Video/Audio 段落 |
| 简介 HTML | `<div id='kdescr'>` | RSS `description` 已是渲染后完整 HTML，直接复用；清洗时去源站 logo/脚本/引用 |
| 文件列表 | `viewfilelist.php?id=<id>` | 解析 `<tr class=rowfollow>` 表格，或 `<FileList>` XML |
| 分类 | 详情页 `"基本信息"` 行 | 也可用 RSS `category`（名称 + 原始 ID） |
| IMDb | 详情页 `tt\d{6,}` | 页面内首个 IMDb tt 号 |
| 豆瓣 | 详情页 `douban.com/subject/\d+` | 从简介/链接提取豆瓣纯数字 ID |
| 种子文件 | `download.php?downhash=` | RSS enclosure 直链（downhash）；也支持 `download.php?id=<id>&passkey=<passkey>` 与 cookie 登录下载 |
| info_hash | bencode SHA1 | 对 `info` dict 做 SHA1，40 位小写 hex；用作全流程去重键 |

> 抓取注意：
> - `details.php?id=<id>` 未登录会 302 到 `login.php`；需带登录 cookie。
> - `viewfilelist.php?id=<id>` 未登录返回 **200 但空 body**（不跳转），需带登录 cookie 才能拿到内容。
> - RSS `<guid>` 字段即 info_hash hex，可直接作去重键（`source.GuidToInfohash`）。

---

## 2. 各目标站必填/可选对照表

三种适配器共享字段映射引擎（`BuildUploadFields` → `mapByType`），
差异仅在**字段名**与**值编码**。

| 字段 | nexusphp API | nexusphp classic | mteam |
|------|-------------|-----------------|-------|
| name | ✅ 必填 | ✅ 必填 | ✅ 必填 |
| descr | ✅ 必填 | ✅ 必填 | ✅ 必填 |
| type / category | ✅ 必填 `type` | ✅ 必填 `type` | ✅ 必填 `category` |
| small_descr | 可选 `small_descr` | 可选 `small_descr` | 可选 `smallDescr`（驼峰） |
| technical_info | `mediainfo` 字段 | ✅ 独立 `technical_info` textarea | `mediainfo` 字段 |
| standard / codec / audiocodec | ✅ taxonomy ID（独立字段） | ✅ `*_sel[4]` select（传可读值） | ✅ 枚举 ID（`standard`/`videoCodec`/`audioCodec`） |
| tags / labels | ✅ `tags[]` 数组 | ✅ `tags[N][]` checkbox | ✅ `labels` + `tags`（逗号拼接） |
| anonymous / uplver | `uplver`（可选） | `uplver=yes`（bool→"yes"） | ✅ 必填 boolean `anonymous` |
| imdb / url | `url`（**纯数字**，去 `tt`） | `url`（**纯数字**，去 `tt`） | `imdb`（**保留 `tt`**） |
| douban | `douban` 字段 | 并入 `descr` | `douban` 字段 |
| 制作组 team | `team`（taxonomy ID） | `team_sel[4]` select | `team`（ID，支持名称→ID 解析） |

**值编码差异**（`convert` 函数）：

| 类型 | nexusphp API | classic | mteam |
|------|-------------|---------|-------|
| bool | `"True"` / `"False"`（Python 风格） | `"yes"` / `""` | `"true"` / `"false"` |
| 数组 | 展开为多个同名字段（`tags[]`） | 逗号拼接 | 逗号拼接 |

**字段映射总览**（`CATEGORY_FIELD_BY_TARGET` + `mapByType`）：

- nexusphp API：`type`/`small_descr`/`url`/`source`/`medium`/`codec`/`standard`/
  `audiocodec`/`team`/`processing`/`tags`/`uplver`。
- classic：`type`/`small_descr`/`url`/`uplver`；**标签与维度并入 descr**
  （`[标签:...]`、`[参数:...]`），因其表单无独立字段。
- mteam：`category`/`smallDescr`/`imdb`/`douban`/`source`/`medium`/`standard`/
  `videoCodec`/`audioCodec`/`team`/`processing`/`labels`/`tags`/`countries`/
  `mediainfo`/`anonymous`。

**必填校验**（`UploadTorrent` 内）：

| 适配器 | 必填校验 |
|--------|----------|
| nexusphp API | `name`、`descr`、`type` |
| nexusphp classic | `name`、`descr`、`type`（另需 cookie） |
| mteam | `name`、`descr`、`anonymous`（`category` 由上层保证） |

---

## 3. 站点差异速查

以下为实际生产站点（dev站 = 传统 NexusPHP 表单，luckpt = NexusPHP Laravel API，
mteam = M-Team）观察到的差异。**均为该站表单/API 的真实值**，与代码内置默认
枚举（`NEXUSPHP_*` / `MTEAM_*`，仅作兜底）可能不同，改站时以本表为准。

| 差异点 | dev站 (classic) | luckpt (API) | mteam |
|--------|----------------|-------------|-------|
| codec H.265 ID | 2 | 6 | 16 |
| standard 4K ID | 4 | 6 | 6 |
| audiocodec AAC ID | 14 | 6 | 6 |
| medium WEB-DL ID | 4 | 11 | —（无默认 medium 维度） |
| 标签 zone | `[4]`（`tags[4][]`） | `tags[]` | `labels` + `tags` |
| 团队 StarfallWeb | `team_sel[4]=8` | `team=10` | `team=59` |

> 说明：
> - classic 的维度是 **select 选项值**（`standard_sel[4]` / `video_codec_sel[4]` /
>   `audio_codec_sel[4]`），站点内部再映射为对应选项 ID；代码侧以可读字符串
>   （`H.265`、`4K`/`2160p` 等）提交。
> - mteam 的 `medium` 维度无内置枚举，标题解析不产出；仅当显式提供 `medium`
>   时才会上传。
> - 表中 dev站/luckpt 的数值来自该站真实表单/API，若代码内置枚举（`NEXUSPHP_*`
>   常量，例如 H.265=2、4K=4、AAC=14）与新站不符，需在站点配置中覆盖。

---

## 4. 字段优先级与数据流

**字段优先级**（低 → 高）：

```
目标站默认/标题自动解析  <  站点 API 分类枚举  <  meta 覆盖(小写别名/category_name)
                        <  cfg 覆盖(extraKeys)  <  parsed 提取
```

具体顺序（`targets.Upload` → `BuildUploadFields`）：

1. 从 `cfg` 的 `extraKeys`（`category`/`small_descr`/`imdb`/`douban`/`source`/
   `medium`/`codec`/`standard`/`processing`/`team`/`audiocodec`/`tags`/`uplver`/
   `anonymous`/`countries`/`labels`/`mediainfo` 等）收集覆盖字段；
2. `meta` 的 `metaOverrideKeys`（`name`/`descr`/`small_descr`/`imdb`/`url`/
   `category_name`/`category`/`anonymous`/`countries`/`labels`/`mediainfo`/`tags`）
   **优先于** cfg 同名字段；
3. 标题：`extra.name` > `parsed.Name`，再按站点类型规范化
   （mteam → `CleanMTteamTitle`；nexusphp → 空格化 `spaceSplitTitle`）；
4. 分类：`extra.category` > `parsed` 分类名 → `ResolveCategoryID`
   （纯数字直通 → 站点 API 分类名 → 内置别名/默认映射 → fallback）；
5. 维度：显式 `extra` 优先，缺失时从标题自动解析。

**转种数据流**：

```
源站 RSS ──→ RssItem(title/descr/category/size/guid)
   │  +details.php(id) ──→ small_descr / tags / imdb / 分类
   │  +viewfilelist.php   ──→ 文件列表
   │  +download.php?downhash= ──→ .torrent
   ▼
parser.ParseTorrent ──→ ParsedTorrent(info_hash/name/files/...)
   │  bencode SHA1 = 去重键
   ▼
targets.Upload ──→ BuildUploadFields ──→ 目标站表单字段
   │   名称规范化 / 副标题构造 / 维度解析 / 分类映射
   ▼
site.UploadTorrent ──→ 上传(鉴权头 + multipart) ──→ target_id
```

**去重键**：全程使用**源站 info_hash**（`parsed.InfoHash`），入库到
`store.relay_jobs`，已存在则跳过。目标站服务端去重（`UploadError.Existing=true`）
记为 `skipped_existing`，同样不重复上传。
