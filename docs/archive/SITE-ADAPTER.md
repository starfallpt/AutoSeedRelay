# 站点适配指南 — 把新站点接入 AutoSeedRelay

> 本文基于 Python 版转种的实战经验,结合 Go 版适配器实现(`internal/targets/*`)写成。
> 目标是:给你一个任意 PT 站的 URL + 凭据,按本文的流程走完,就能把它作为目标站接入转种。
>
> 已实测站点:dev.internal-source.org(传统表单)、luckpt.de(NexusPHP API)、北洋园 TJUPT(传统表单)、M-Team(自定义 API)。
> 相关代码:`internal/targets/nexusphp.go`、`internal/targets/nexusphp_classic.go`、`internal/targets/mteam.go`、`internal/targets/base.go`。

---

## 目录

1. [适配新站的第一步:探测](#1-适配新站的第一步探测)
2. [上传字段如何确定](#2-上传字段如何确定)
3. [配置文件格式](#3-配置文件格式)
4. [实战案例:dev.internal-source.org(传统表单)](#41-devinternal-sourceorg传统表单)
5. [实战案例:luckpt.de(API)](#42-luckptdeapi)
6. [标签映射怎么做](#43-标签映射怎么做)
7. [接入检查清单](#5-接入检查清单)

---

## 1. 适配新站的第一步:探测

### 1.1 判断站点架构

所有 PT 站基本可以归入下面三种架构,适配器直接对应:

| 架构类型 | 上传方式 | 鉴权 | 适配器 (`SiteType`) | 判定方法 |
|---|---|---|---|---|
| NexusPHP >= 1.9(Laravel 二开) | API | Sanctum Bearer token | `nexusphp` | `GET /api/v1/sections` 返回 JSON |
| 传统 NexusPHP(老版本) | 表单 | Cookie | `nexusphp_classic` | `upload.php` 返回 HTML 表单,`action="takeupload.php"` |
| M-Team(自定义 Spring) | API | `x-api-key` | `mteam` | `POST /api/torrent/categoryList` 返回 JSON |

**判定命令(只读,不会触发上传):**

```bash
# 1) 试探 NexusPHP API 端点(有 token 就带上)
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer <token>" \
  https://<站点>/api/v1/sections
# 200 → NexusPHP >= 1.9 API 站点;404/401 → 不是(或 token 无效)

# 2) 试探传统上传页(带登录 cookie)
curl -s -b "access_token=<cookie>" https://<站点>/upload.php | head -40
# 若返回 <form ... action="takeupload.php"> → 传统 NexusPHP
# 若返回 <select name="type"> → 传统站(分类下拉)

# 3) 试探 M-Team 风格 API
curl -s -X POST -H "x-api-key: <key>" https://api.<站点>/api/torrent/categoryList
# 返回 JSON(含 nameChs/nameCht/nameEng)→ M-Team 系
```

Go 版内置了 `probe` 子命令,自动完成第 1 步并输出分类映射:

```bash
export SITE_URL='https://luckpt.de'
export SITE_TOKEN='<sanctum-bearer-token>'     # 或 SITE_COOKIE='access_token=...'
go run ./cmd/relay probe --out config/local/sites/luckpt.json
# 输出:站点探测结果 + 分类映射 + sections 结构,写入 luckpt.json
```

> 关键判断点:**先确认有没有 `/api/v1/sections`**。有 → 直接走 API 适配器,省去所有表单解析;
> 没有 → 抓 `upload.php` 解析 HTML 表单。M-Team 既没有 sections 也没有 upload.php,只有它自己的 `/api/torrent/*`。

### 1.2 探测上传方式:API vs 表单

一句话:**看有没有 `/api/v1/upload`**。

| 特征 | API 站(nexusphp) | 表单站(nexusphp_classic) |
|---|---|---|
| 上传端点 | `POST /api/v1/upload` | `POST /takeupload.php` |
| 请求体 | multipart,文件字段 `file`,标签 `tags[]` | multipart 表单,字段即 `<select>/<input>` 的 name |
| 响应 | JSON,成功取 `data.id` | 302 到 `details.php?id=N` |
| 去重判定 | HTTP 409 或 body 含 `torrent_existed` 等 | body 含 `种子已存在 / already exists / 重复` |
| 必填字段 | `name`、`descr`、`type` | `name`、`descr`、`type`、`cookie` |

> 注意:**同一个 NexusPHP 站点的生产环境和 dev 环境可能不同**。dev 站往往没开 API 或者版本旧,
> 只暴露 `upload.php`。所以判断依据永远是"探测结果",而不是"站点叫什么"。
> 实战里 dev.internal-source.org 就探测出是传统表单站(见 [4.1](#41-devinternal-sourceorg传统表单))。

### 1.3 需要的凭据

| 架构 | 凭据 | 从哪里拿 | 用在哪个头 |
|---|---|---|---|
| NexusPHP API | **Sanctum Bearer token** | 个人设置页 → 安全 / API 选项卡 | `Authorization: Bearer <token>` |
| 传统 NexusPHP | **登录 Cookie**(`access_token=...`) | 浏览器登录后 F12 → Network → 请求头 | `Cookie: access_token=...` |
| M-Team | **实验室令牌** | 个人设置 → 安全 → API 实验室 | `x-api-key: <key>` |

几点实战经验:

- **token 优先于 cookie**:API 站用 token 更稳定(不随会话过期)。传统站没有 token,只能 cookie。
- **cookie 会过期**:长跑任务建议给传统站配置定时刷新 cookie,或用 API 站替代。
- **凭据一律走环境变量**,不要写进 YAML/JSON 提交到 git(见 [第 3 节](#3-配置文件格式))。

---

## 2. 上传字段如何确定

字段确定的核心原则:

> **永远从目标站自己的上传表单/API 里提取枚举,不要照抄别的站点的数字。**
> 每个站点的 taxonomy ID 都是自己的,同一个语义在不同站点值完全不同。

### 2.1 分类(Category)

分类是**唯一必填**的枚举字段(API 站叫 `type`,M-Team 叫 `category`)。

**API 站**:`GET /api/v1/sections` 返回 JSON,`categories` 可能是嵌套结构(父/子分类)。

```json
{
  "categories": [
    { "id": 401, "name": "电影", "children": [ { "id": 419, "name": "电影-高清" } ] }
  ]
}
```

Go 代码里 `ParseCategoriesMapping` 递归遍历 `{id, name}` / `{id, label}`,自动展平成 `{分类名: id}`(见 `internal/targets/base.go`)。

**传统站**:解析 `upload.php` 里 `<select name="type">` 的 `<option>`。

```html
<select name="type" class="dropdown">
  <option value="401">电影</option>
  <option value="402">剧集</option>
  <option value="405">动漫</option>
</select>
```

**M-Team**:`POST /torrent/categoryList` 返回列表,`parseMTeamCategories` 把简体名/繁体名/英文名都映射到同一个 id(见 `internal/targets/mteam.go`)。

**兜底**:分类推断失败时用 `fallback_category`(见 `internal/targets/targets.go` 的 `ResolveCategoryID`)。

### 2.2 标签(Tags)

标签是**最容易踩坑**的字段,两种架构差别很大:

**API 站**:`/api/v1/sections` 里通常带 `tags` 列表,结构是 `{id, name}`。

```json
{
  "tags": [
    { "id": 5,  "name": "国语" },
    { "id": 6,  "name": "中字" },
    { "id": 7,  "name": "HDR"  }
  ]
}
```

提交时用 `tags[]` 数组(Go 的 `postMultipart` 会把 slice 展开成多个同名字段):

```
tags[]=5&tags[]=6
```

**传统站**:`upload.php` 里标签是一组 checkbox,`name="tags[4][]"`,**value 是数字 ID,不是中文名**。

```html
<input type="checkbox" name="tags[4][]" value="8" />国语
<input type="checkbox" name="tags[4][]" value="9" />中字
```

> 这意味着:传统站**必须**建立"中文标签名 → 数字 ID"的映射表(见 [4.3](#43-标签映射怎么做)),
> 否则你不知道该提交哪个 value。

**当前 Go 实现的取舍**:

| 架构 | 标签处理方式 |
|---|---|
| `nexusphp`(API) | 独立 `tags[]` 数组字段,提交数字 ID |
| `nexusphp_classic`(表单) | **并入 descr**,简介末尾追加 `[标签:国语,中字]` 行 |
| `mteam` | `tags` 字段(逗号分隔的字符串) |

> 传统站之所以并入 descr,是为了省去标签 ID 映射表;但如果目标站后台按标签做筛选,
> 并进 descr 的标签不会生效,此时就值得老老实实建映射表、提交 `tags[4][]`。

### 2.3 维度(Dimensions)

维度指 `standard`(分辨率)/ `codec`(视频编码)/ `audiocodec`(音频编码)/ `medium`(媒介)/ `source`(片源)/ `team`(制作组)。

**每个站点的 taxonomy ID 都不同**,这是适配时最容易错的点。实战例子:

| 语义 | luckpt.de | dev.internal-source.org | M-Team |
|---|---|---|---|
| `standard` 4K/2160p | ?(=某值) | 4 | 6 |
| `codec` H.265/HEVC | **6** | **2** | 16 |
| `codec` H.264 | ? | 1 | 1 |
| `audiocodec` E-AC3/DDP | ? | 11 | 12 |

> 来源站(源站)的 taxonomy ID 又和上面完全不同。所以**源站的值不能直接透传**,
> 必须做 源站语义 → 目标站 ID 的映射。

Go 代码里内置了两套枚举(见 `internal/targets/base.go`):

```go
// 星陨阁(nexusphp-api)实测
var NEXUSPHP_STANDARD = map[string]int{ "1080p": 3, "720p": 2, "2160p": 4, "4K": 4, "SD": 6 }
var NEXUSPHP_VIDEO_CODEC = map[string]int{ "H.264": 1, "H.265": 2, "HEVC": 2, "AV1": 16 }

// M-Team 实测
var MTEAM_STANDARD = map[string]int{ "1080p": 1, "720p": 3, "SD": 5, "4K": 6, "2160p": 6, "8K": 7 }
var MTEAM_VIDEO_CODEC = map[string]int{ "H.264": 1, "H.265": 16, "HEVC": 16, "AV1": 19 }
```

接入新站时,维度枚举从两个来源拿:

1. **API 站**:`/api/v1/sections` 里通常有 `standard_list` / `codec_list` / `audiocodec_list` / `medium_list` / `source_list` / `team_list` 之类的 taxonomy 列表。
2. **传统站**:`upload.php` 里对应的 `<select name="standard_sel[...]">` 等下拉框。

**传统站的维度提交差异**:老 NexusPHP 通常**没有独立维度字段**(或字段名带 zone 后缀,见 2.5),
当前 Go 实现把维度并入 descr:

```
[参数:1080p,H.264,AC3]
```

即 `[参数:<standard>,<video_codec>,<audio_codec>]`,见 `mapByType` 里 `nexusphp_classic` 分支。

### 2.4 MediaInfo

| 架构 | 字段 | 说明 |
|---|---|---|
| API 站(nexusphp) | `mediainfo` | 部分二开站有独立的 MediaInfo 字段 |
| 传统站 | `technical_info` textarea | 在 `upload.php` 表单里,textarea name 通常是 `technical_info` |
| M-Team | `mediainfo` | 独立字段 |

MediaInfo 内容从源站详情页抓取(`details.php` 的 mediainfo 区块),转种时作为纯文本带过去。
`mediainfo` 已在 Go 的 extra 覆盖键里(`internal/targets/targets.go` 的 `extraKeys`)。

### 2.5 zone ID(传统站的 `*_sel[4]` vs `*_sel[6]`)

传统 NexusPHP 的上传表单里,维度下拉框的 name 带一个 **zone 后缀**(PHP 数组下标),
**不同的分类区用不同的后缀**:

```html
<!-- 电影区(如分类 4xx) -->
<select name="standard_sel[4]">...</select>
<select name="codec_sel[4]">...</select>

<!-- 另一组分类区(如 6xx) -->
<select name="standard_sel[6]">...</select>
<select name="codec_sel[6]">...</select>
```

> 含义:一个站点的分类被分成若干"区"(zone),每个区有自己的一套维度下拉。`[4]` 和 `[6]`
> 就是两个不同区的维度枚举,值可能不同。
>
> **提交时必须把维度值放进与目标分类同区的 `*_sel[N]` 字段**,否则分类与维度对不上,
> 上传会被拒绝或产生脏数据。

判断方法:解析 `upload.php`,看你要选的分类(如 `type=401` 电影)对应哪一组 `*_sel[N]`,
然后从那一组里取维度 ID。

---

## 3. 配置文件格式

配置文件默认读 `config/relay.yaml`(兜底 `config/relay.json`),结构如下。
**敏感字段一律用 `<PUT_ENV_XXX>` 占位符 + 环境变量覆盖,不落盘真实凭据。**

```yaml
# config/relay.yaml
#
# 加载规则(见 internal/config/config.go):
#   1. 读文件;
#   2. 递归替换 <PUT_ENV_XXX> 占位符(用环境变量值,未设置则报错提示缺哪个变量);
#   3. 按 AUTOSEED_<SITE>_<FIELD> 用环境变量覆盖(环境变量优先于文件值);
#   4. 校验。

# ---------- 源站(RSS 抓取) ----------
sources:
  - name: SOURCE                      # 站点标识,小写,不要用下划线
    base_url: https://dev.internal-source.org
    rss_url: https://dev.internal-source.org/torrentrss.php?passkey=<PUT_ENV_AUTOSEED_SOURCE_PASSKEY>&rows=20&linktype=dl
    cookie: <PUT_ENV_AUTOSEED_SOURCE_COOKIE>     # 抓详情/文件列表需要
    # api_token: <PUT_ENV_AUTOSEED_SOURCE_TOKEN> # 若源站也是 API 站,可选

# ---------- 目标站(上传) ----------
targets:
  # 例 1:NexusPHP API 站(luckpt.de)
  - name: luckpt
    base_url: https://luckpt.de
    announce_url: https://luckpt.de/announce.php?passkey={passkey}
    api_token: <PUT_ENV_AUTOSEED_LUCKPT_TOKEN>
    extra:
      target: nexusphp                 # 适配器类型,见 targets.go 注册表
      fallback_category: 401           # 分类推断失败时的兜底
      categories:                      # 可选:预置分类映射(不拉 API 也能 dry-run)
        电影: 401
        剧集: 402
        动漫: 405
      tags_map:                        # 可选:标签名 → 目标站数字 ID
        国语: 5
        中字: 6

  # 例 2:传统 NexusPHP 表单站(dev.internal-source.org)
  - name: SOURCE-target
    base_url: https://dev.internal-source.org
    announce_url: https://dev.internal-source.org/announce.php?passkey={passkey}
    cookie: <PUT_ENV_AUTOSEED_SOURCE_TARGET_COOKIE>
    extra:
      target: nexusphp_classic         # 传统表单适配器
      fallback_category: 401
      categories:
        电影: 401
        剧集: 402

  # 例 3:M-Team
  - name: mteam
    base_url: https://api.m-team.cc/api
    announce_url: https://tracker.m-team.cc/announce?credential={credential}
    mteam_auth: <PUT_ENV_AUTOSEED_MTEAM_AUTH>       # x-api-key 头

# ---------- 全局参数(可选,均有默认) ----------
poll_interval: 300               # 轮询间隔(秒)
keywords: [StarfallWeb, LongWeb] # 标题命中关键词(列表或逗号分隔字符串)
db_path: data/relay.db           # 去重/状态 SQLite
out_dir: data/out                # 种子输出目录
```

环境变量覆盖(敏感字段分离):

```bash
# 方式一:文件里写占位符,shell 里设环境变量(自动替换)
export AUTOSEED_SOURCE_PASSKEY='真实passkey'
export AUTOSEED_LUCKPT_TOKEN='真实sanctum-token'
export AUTOSEED_SOURCE_TARGET_COOKIE='access_token=...'

# 方式二:纯环境变量构造,不写配置文件
export AUTOSEED_TARGET_LUCKPT_BASE_URL='https://luckpt.de'
export AUTOSEED_TARGET_LUCKPT_API_TOKEN='...'
export AUTOSEED_TARGET_LUCKPT_ANNOUNCE_URL='https://luckpt.de/announce.php?passkey={passkey}'
```

> 站点 `name` 决定环境变量 token:`SOURCE` → `SOURCE`,`m-team` → `M_TEAM`。
> 字段别名:`TOKEN`/`API_TOKEN` → `api_token`,`AUTH` → `mteam_auth`,`COOKIE` → `cookie`,
> `ANNOUNCE`/`ANNOUNCE_URL` → `announce_url`,详见 `internal/config/config.go` 的 `envFieldAliases`。

---

## 4. 实战案例

### 4.1 dev.internal-source.org(传统表单)

**背景**:目标是把它作为目标站,把源站种子转种上传。

**Step 1 — 探测**

```bash
# 先试 API(带 token)
curl -s -o /dev/null -w "%{http_code}\n" https://dev.internal-source.org/api/v1/sections
# → 404 / 401,API 不可用

# 改抓上传页
curl -s -b "access_token=<cookie>" https://dev.internal-source.org/upload.php | head -60
```

`upload.php` 返回传统 NexusPHP 表单:

```html
<form name="upload" method="post" action="takeupload.php" enctype="multipart/form-data">
  <select name="type">
    <option value="401">电影</option>
    <option value="402">剧集</option>
    <option value="405">动漫</option>
    <option value="406">音乐</option>
    <option value="410">其他</option>
  </select>
  <input type="text" name="name" />
  <textarea name="descr"></textarea>
  <input type="text" name="small_descr" />
  <input type="text" name="url" />
  <input type="checkbox" name="tags[4][]" value="8" />国语
  <input type="checkbox" name="tags[4][]" value="9" />中字
  <select name="standard_sel[4]"><option value="3">1080p</option>...</select>
  <select name="codec_sel[4]"><option value="2">H.265/HEVC</option>...</select>
  <textarea name="technical_info"></textarea>
</form>
```

**结论**:

| 探测项 | 结果 |
|---|---|
| 架构 | 传统 NexusPHP → 适配器 `nexusphp_classic` |
| 上传端点 | `POST /takeupload.php` |
| 鉴权 | Cookie(`access_token=...`) |
| 分类 | `type`:401=电影, 402=剧集, 405=动漫, 406=音乐, 410=其他 |
| 标签 | `tags[4][]`,value=8(国语)、9(中字)—— **数字 ID** |
| 维度 | `standard_sel[4]`(1080p=3)、`codec_sel[4]`(H.265=2) |
| MediaInfo | `technical_info` textarea |
| 成功判定 | 302 到 `details.php?id=N` |

**Step 2 — 写配置**(见第 3 节例 2)

**Step 3 — dry-run 预览字段**

```bash
go run ./cmd/relay upload --torrent /tmp/src.torrent \
  --target nexusphp_classic \
  --category 电影 --dry-run --workdir /tmp/dryrun
```

预期输出(分类=401,维度并入 descr):

```yaml
=== nexusphp_classic dry-run 字段 ===
  name: Example.Movie.2026.1080p.WEB-DL.x264
  descr: |
    <源站简介...>
    [标签:国语,中字]
    [参数:1080p,H.264,AC3]   # 顺序:standard,video_codec,audio_codec(并入简介)
  type: 401
  small_descr: ...
  url: 1234567
  upload_url = https://dev.internal-source.org/takeupload.php
```

**Step 4 — 真实上传 1 个种子**,与站点"我的发布"逐字段对比(标题/分类/标签/简介),确认后台展示无误后再批量跑。

> **关键经验**:传统站的 `type`(分类)是必填且直接影响维度 zone(见 2.5)。
> 如果上传后提示分类与维度不匹配,检查 `type` 对应的 zone 后缀是不是 `[4]`/`[6]`。

### 4.2 luckpt.de(API)

**背景**:luckpt.de 是 NexusPHP >= 1.9(Laravel)二开站,开放了 API。

**Step 1 — 探测**

```bash
export SITE_URL='https://luckpt.de'
export SITE_TOKEN='<sanctum-bearer-token>'
go run ./cmd/relay probe --out config/local/sites/luckpt.json
```

`/api/v1/sections` 返回 JSON,包含分类、tags、以及各维度 taxonomy 列表:

```json
{
  "categories": [
    { "id": 401, "name": "电影" },
    { "id": 402, "name": "剧集" },
    { "id": 405, "name": "动漫" }
  ],
  "tags": [
    { "id": 5,  "name": "国语" },
    { "id": 6,  "name": "中字" },
    { "id": 7,  "name": "HDR" }
  ],
  "codec_list": [
    { "id": 1, "name": "H.264" },
    { "id": 6, "name": "H.265/HEVC" },
    { "id": 7, "name": "AV1" }
  ],
  "standard_list": [
    { "id": 3, "name": "1080p" },
    { "id": 4, "name": "2160p" }
  ]
}
```

**结论**:

| 探测项 | 结果 |
|---|---|
| 架构 | NexusPHP API → 适配器 `nexusphp` |
| 上传端点 | `POST /api/v1/upload` |
| 鉴权 | `Authorization: Bearer <token>`(Sanctum) |
| 分类 | `type`:401=电影, 402=剧集, 405=动漫 |
| 标签 | 独立 `tags[]` 数组,value=5(国语)、6(中字) |
| 维度 | **注意:`codec_list` 里 H.265/HEVC=6**,和 dev 站(=2)、M-Team(=16)都不一样 |
| MediaInfo | `mediainfo` 独立字段 |

> 这就是"每个站点 taxonomy 不同"的最典型例子:同样语义 H.265/HEVC,
> **luckpt.de=6,dev.internal-source.org=2,M-Team=16**。三个站三个值,必须各自提取。

**Step 2 — 写配置**(见第 3 节例 1)

**Step 3 — dry-run**

```bash
go run ./cmd/relay upload --torrent /tmp/src.torrent \
  --target nexusphp --category 电影 --dry-run --workdir /tmp/dryrun
```

预期输出(API 站有独立维度字段;`codec` 必须用 luckpt 自己的枚举):

```yaml
=== nexusphp dry-run 字段 ===
  name: Example Movie 2026 2160p WEB-DL H.265 DDP5.1
  descr: <源站简介...>
  type: 401
  codec: 6        # H.265/HEVC —— luckpt codec_list 的 ID
  standard: 4     # 2160p
  source: 4       # WEB
  medium: 4       # WEB
  audiocodec: 11  # DDP
  tags: [5, 6]    # 国语,中字
  upload_url = https://luckpt.de/api/v1/upload
```

> **注意**:Go 内置的 `NEXUSPHP_VIDEO_CODEC` 里 H.265=2(那是 dev/星陨阁的实测值),
> 而 luckpt 是 6。接入新站时**必须用目标站 sections 里自己的 `codec_list` 覆盖内置默认值**,
> 否则会按 dev 站的 ID 上传,后台编码显示错误。

**Step 4 — 真实上传 1 个**,确认后台分类/编码/标签显示正确。

> **关键经验**:API 站虽然省了表单解析,但维度 ID 照样要按本站的 `*_list` 来。
> 不要假设"所有 NexusPHP 都是同一套 ID"。源站的分类 ID / 维度 ID 一律不能透传。

### 4.3 标签映射怎么做

传统站(以及个别 API 站)的标签提交值是**数字 ID**,而源站详情页/上游解析出来的是**中文名**。
所以要做一张"源站标签名 → 目标站标签数字 ID"的映射表。

**做法**:

1. 从目标站提取标签枚举:
   - API 站:`/api/v1/sections` 的 `tags` 列表 `{id, name}`
   - 传统站:解析 `upload.php` 的 `tags[4][]` checkbox(value 是 ID,标签文字是 name)
2. 建映射表(以下为示例值,以实际探测为准):

| 源站标签名(标准化) | luckpt.de `tags` ID | dev.internal-source.org `tags[4]` ID | M-Team 标签文本 |
|---|---|---|---|
| 国语 | 5 | 8 | 国语 |
| 中字 | 6 | 9 | 中字 |
| 英字 | 12 | 10 | 英字 |
| 粤语 | 15 | 11 | 粤语 |
| HDR | 7 | 12 | HDR |
| 特效 | 20 | 13 | 特效 |
| DIY | 22 | 14 | DIY |
| 杜比 | 25 | 15 | 杜比 |

3. 映射放进目标站配置的 `tags_map`(见第 3 节)或写死在适配器里:

```yaml
extra:
  tags_map:
    国语: 5      # luckpt.de 场景:提交 tags[]=5
    中字: 6
    英字: 12
```

4. 传统站如果不想维护映射表,可以接受"标签并入 descr"(Go 的 `nexusphp_classic` 默认行为)。
   但如果目标站后台按标签筛选,就必须走数字 ID 提交。

---

## 5. 接入检查清单

- [ ] 探测:确认架构(API / 传统表单 / M-Team),`relay probe` 或 curl 跑通
- [ ] 上传端点确认:`/api/v1/upload` vs `takeupload.php` vs `/torrent/createOredit`
- [ ] 鉴权确认:Bearer token / Cookie / x-api-key
- [ ] 分类枚举拿到(API `sections` / 表单 `<select name="type">` / `categoryList`)
- [ ] 标签枚举拿到,**value 是数字 ID**(API `tags` / 表单 `tags[4][]`)
- [ ] 维度枚举按**本站**提取(API `*_list` / 表单 `*_sel[N]`),不照抄别的站
- [ ] 传统站确认 zone 后缀:`type` 分类对应的 `*_sel[4]` 还是 `*_sel[6]`
- [ ] MediaInfo 字段确认(API `mediainfo` / 传统 `technical_info`)
- [ ] 配置文件写占位符,真实凭据走环境变量,不提交 git
- [ ] `dry-run` 预览字段,核对 name/descr/type/维度/tags
- [ ] 真实上传 1 个 → 与"我的发布"逐字段对比 → 再批量
- [ ] 固化分类/维度/标签枚举到 `internal/targets/base.go` + 写测试

---

## 相关文档

- [站点间参数映射指南](SITE-MAPPING-GUIDE.md) — 标准化键体系 + per-site YAML 映射
- [Python 版代码收束参考](python-consolidation.md) — Python 版全部模块的紧凑归纳
- [README](../README.md) — 快速开始与环境变量
