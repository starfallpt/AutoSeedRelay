# M-Team API 参考

> 基于实战经验整理。M-Team 目标站后端为 Spring Boot，接口走 JSON，
> 上传为 `multipart/form-data`。对应 Go 实现：`internal/targets/mteam.go`。
>
> `api_base` 默认为 `https://api.m-team.io/api`，下文所有 `/api/...` 端点
> 均为相对该基址的路径（代码中通过 `apiBase + "/torrent/xxx"` 拼接）。

---

## 1. 鉴权

M-Team 使用**实验室令牌**（lab token / API key），通过请求头传递：

```
x-api-key: <实验室令牌>
```

关键点（实战踩坑）：

- **不是** `Authorization: Bearer <token>`（那是 NexusPHP API 的写法）。
- **不是** URL 参数 `ts=<token>`。
- 令牌通过适配器配置字段 `auth_token`（别名 `api_token`）注入，见
  `MTeamAPI.ApplyConfig`。
- dry-run / 无凭据时，代码仍会带上空的 `x-api-key` 头占位，避免漏头。

示例：

```
GET /api/torrent/categoryList
Host: api.m-team.io
x-api-key: <实验室令牌>
User-Agent: AutoSeedRelay/0.1 (+relay script)
```

---

## 2. 端点

### 2.1 `POST /api/torrent/createOredit` — 上传种子

- 方法：`POST`
- Content-Type：`multipart/form-data`
- 鉴权：`x-api-key` 头

**必填字段**

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 种子标题（M-Team 规范标题） |
| `descr` | string | 简介 HTML |
| `category` | int | 分类 ID（见 [分类 ID](#3-分类-id)） |
| `anonymous` | bool | 是否匿名发布，必须显式给出（代码默认 `false`） |
| `file` | file | .torrent 文件，字段名固定为 `file` |

> 注：Go 适配器在 `UploadTorrent` 内校验必填 `name`/`descr`/`anonymous`，
> `category` 由上层 `Upload` 入口保证（缺失时报“无法确定分类”）。

**可选字段**

| 字段 | 类型 | 说明 |
|------|------|------|
| `smallDescr` | string | 副标题（注意 M-Team 是驼峰 `smallDescr`，不是 `small_descr`） |
| `imdb` | string | IMDb ID，**保留 `tt` 前缀**（如 `tt6485574`） |
| `douban` | string | 豆瓣 ID（纯数字） |
| `standard` | int | 分辨率枚举 ID（见 [维度 ID](#4-维度-id)） |
| `videoCodec` | int | 视频编码枚举 ID |
| `audioCodec` | int | 音频编码枚举 ID |
| `source` | int | 片源枚举（若目标站有该维度） |
| `medium` | int | 介质枚举（若目标站有该维度） |
| `team` | int | 制作组 ID（支持名称→ID 解析） |
| `labels` | list | 标签/标签组，逗号拼接提交 |
| `tags` | list | 标签，逗号拼接提交 |
| `countries` | list | 国家/地区，逗号拼接提交 |
| `mediainfo` | string | MediaInfo 全文 |
| `processing` | int | 处理方式（可选） |

**字段值编码规则**（`mteamConvert`）：

- bool → `"true"` / `"false"` 小写字符串；
- 数组/列表（`labels`/`tags`/`countries`）→ 逗号拼接的单个字段；
- 其余 → 字符串化。

**成功响应**

```json
{
  "code": 0,
  "data": {
    "id": 123456
  }
}
```

- 判定成功：`code == 0` 或 `data.id` 存在。
- `data.id` 即目标站种子 ID（`target_id`）。

**失败 / 去重**

- HTTP 非 2xx，或响应体命中去重关键词 → 抛 `UploadError`（见
  [去重检测](#5-去重检测)）。

### 2.2 `POST /api/torrent/categoryList` — 获取分类列表

- 方法：`POST`（无请求体）
- 返回：`{"code":0,"data":[...]}`，`data` 为分类数组；也可能为
  `{"list":[...]}` 或 `{"categories":[...]}` 包裹。
- 每个分类对象含 `id` 与多个名字键：`nameChs`（简体）、`nameCht`（繁体）、
  `nameEng`、`name`、`label`。解析时把**任意语言的名称都映射到同一个 id**，
  便于按中文/英文任意匹配。
- 适配器缓存到内存，`GetCategories()` 只拉取一次。
- 未配置 `api_base`（dry-run）时回退到内置默认映射（见 [分类 ID](#3-分类-id)）。

### 2.3 `POST /api/torrent/teamList` — 获取制作组列表

- 方法：`POST`（无请求体）
- 返回：`{"code":0,"data":[{"id":59,"name":"StarfallWeb"}, ...]}`
- 解析为 `{制作组名: id}`，适配器缓存到内存，`GetTeams()` 只拉取一次。
- 拉取失败时静默返回空映射（上传时团队按名称→ID 解析失败则原样透传）。

---

## 3. 分类 ID

官方分类 ID（实测自生产 API，`internal/targets/base.go` 的 `MTeamCategoryID`）。

### 顶级分类

| 分类 | ID |
|------|----|
| movie（电影） | 100 |
| tv（剧集） | 105 |
| doc（纪录片） | 404 |
| anime（动漫） | 405 |
| music（音乐） | 110 |
| other（其它） | 409 |

### 细分分类

| 分类 | ID |
|------|----|
| movie_hd（电影 HD） | 419 |
| movie_bluray（电影原盘/蓝光） | 421 |
| movie_remux（电影 Remux） | 439 |
| movie_sd（电影 SD） | 401 |
| movie_dvdiso（电影 DVD ISO） | 420 |
| tv_series（剧集 合集） | 448 |
| tv_hd（剧集 HD） | 402 |
| tv_bluray（剧集蓝光） | 438 |
| tv_sd（剧集 SD） | 403 |
| tv_dvdiso（剧集 DVD ISO） | 435 |

**别名**：解析时支持中英文别名归一（`MTeamCategoryAlias`），例如
`电影/影片` → `movie`，`剧集/电视剧/连续剧/综艺` → `tv`，`动漫/动画` → `anime`
等。若站点 API 已返回实时分类，优先用实时值，内置映射仅作兜底。

---

## 4. 维度 ID

枚举 ID 实测自生产 API（`internal/targets/base.go`）。字段名与枚举名对应：
`standard` → `MTEAM_STANDARD`，`videoCodec` → `MTEAM_VIDEO_CODEC`，
`audioCodec` → `MTEAM_AUDIO_CODEC`。

### standard（分辨率）

| 标准名 | 别名 | ID |
|--------|------|----|
| 1080p | 1080p | 1 |
| 1080i | 1080i | 2 |
| 720p | 720p | 3 |
| SD | SD | 5 |
| 4K | 2160p | 6 |
| 8K | 8K | 7 |

### videoCodec（视频编码）

| 标准名 | 别名 | ID |
|--------|------|----|
| H.264 | H264 / X264 / AVC | 1 |
| VC-1 | VC1 | 2 |
| XVID | XviD | 3 |
| MPEG-2 | MPEG2 | 4 |
| H.265 | H265 / HEVC / X265 | 16 |
| AV1 | AV1 | 19 |
| VP8 | VP8 | 21 |
| VP9 | VP9 | 21 |
| AVS | AVS | 22 |

### audioCodec（音频编码）

| 标准名 | 别名 | ID |
|--------|------|----|
| FLAC | FLAC | 1 |
| APE | APE | 2 |
| DTS | DTS | 3 |
| MP2 | MP3 | 4 |
| OGG | OGG | 5 |
| AAC | AAC | 6 |
| OTHER | 其他 | 7 |
| AC3 | DD | 8 |
| TRUEHD | TrueHD | 9 |
| TRUEHD ATMOS | TrueHD Atmos | 10 |
| DTS-HD MA | DTSHDMA | 11 |
| DDP | E-AC3 / EAC3 | 12 |
| DDP ATMOS | E-AC3 ATOMS | 13 |
| LPCM | PCM | 14 |
| WAV | WAV | 15 |

> 注意：`DTS` 是 3、`AC3` 是 8、`DDP` 是 12，别和 NexusPHP 的枚举搞混。

**维度自动解析**：若上传字段未显式给出 `standard`/`videoCodec`/`audioCodec`，
适配器会从标题中按 token 匹配（`resolveMTeamDimensions`）自动补全，例如标题含
`4K` → `standard=6`，`HEVC` → `videoCodec=16`，`DDP5.1` → `audioCodec=12`。

---

## 5. 去重检测

上传失败时，通过响应体关键词判定是否为“种子已存在”。命中即标记
`UploadError.Existing = true`，由流水线记为 `skipped_existing`（不视为硬错误）。

完整关键词表（`mteamExistingHints`，英文按小写匹配，含繁体）：

| 类型 | 关键词 |
|------|--------|
| 英文 | `duplicate` |
| 英文 | `exists` |
| 英文 | `existed` |
| 英文 | `repeat upload` |
| 英文 | `already uploaded` |
| 繁体 | `種子已存在` |
| 简体 | `种子已存在` |
| 简/繁 | `重复发布` / `重複發布` |
| 通用 | `已存在` |
| 通用 | `已上传过` / `已上傳過` |

判定逻辑：响应体 `ToLower()` 后逐个关键词 `Contains`，任一命中即视为已存在。

---

## 6. 公告 URL

M-Team **没有 passkey**。tracker 地址为：

```
https://tracker.m-team.cc/announce?credential={credential}
```

- 占位符 `{credential}` 中的真实凭证由**服务端在上传时自行改写**——上传的
  .torrent 里 announce 只需带占位值即可，服务端会替换为对应用户的 credential。
- 因此适配器 `BuildAnnounce()` 在 dry-run 时把 `{credential}` 替换成
  `PLACEHOLDER` 占位，不改写任何真实凭证。
- 这也意味着清洗种子（`CleanTorrentForTarget`）改 announce 时不需要（也不应）
  注入 passkey，与 NexusPHP 系 `announce.php?passkey=...` 的做法完全不同。

---

## 7. 常见坑速查

| 坑 | 正确做法 |
|----|----------|
| 鉴权头写成了 `Authorization: Bearer` | 用 `x-api-key` 头 |
| 副标题字段名写 `small_descr` | M-Team 用驼峰 `smallDescr` |
| IMDb 传纯数字 | M-Team 要保留 `tt` 前缀（`imdb` 字段） |
| 分类字段写 `type` | M-Team 用 `category`（见 `CATEGORY_FIELD_BY_TARGET`） |
| 匿名不传 | `anonymous` 是必填，必须显式 `true`/`false` |
| 分类用英文名直传 | 需先经 `categoryList` 或内置映射转成整数 ID |
