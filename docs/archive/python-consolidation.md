# AutoSeedRelay Python 代码收束参考文档

> 本文档是全部 Python 源码的紧凑归纳，供 Go 重写代理直接参考，**无需再读原始 .py 文件**。

---

## 项目概览

PT 站自动转种流水线：源站 RSS → 关键词筛选 → 下载 .torrent → 解析 bencode → qB 做种 → 清洗种子 → 上传目标站 → 去重入库

状态机: `pending → downloaded → added_to_qb → seeded → uploaded → cross_seeded` (任意阶段可 `failed/skipped`)

去重键: **源站 info_hash** (RSS `<guid>` 字段)

---

## 模块清单 (17 个源文件, ~5800 行)

### 1. bencode.py (136行) — Bencode 编解码
**纯自研，不依赖任何库。** 递归下降解析器。

公开 API:
- `BencodeError(ValueError)`
- `decode(data: bytes) -> Any` — 严格模式：截断数据/非法标记/尾部数据均报错
- `encode(value: Any) -> bytes` — 支持 int/bytes/str/list/dict；**dict key 按字节序排序**（保证 info_hash 稳定）
- `info_hash_of(torrent: dict) -> str` — SHA1(bencode(torrent[b"info"])) → 40 位小写 hex
- `load_torrent(path) -> dict` — 读文件+解码，要求顶层 dict 含 `b"info"` key
- `write_torrent(path, torrent) -> None`

**Go 要点**: 实现 `Marshal/Unmarshal`，key 排序用 `sort.Strings`，info_hash 计算 SHA1

### 2. parser.py (152行) — 种子解析与清洗
公开 API:
- `ParsedTorrent` dataclass: path, info_hash(40hex), name, announce, total_size, file_count, files[{path,size}], is_private, source, is_v2, raw_dict
- `parse_torrent(path) -> ParsedTorrent` — 拒绝 v2/hybrid (检测 `meta version==2`, `piece layers`, `files tree`)
- `clean_torrent_for_target(torrent, *, target_announce, target_site_name, target_base_url, jitter_range=(600,1200)) -> dict` — deepcopy 后:
  1. announce → target_announce (bytes)
  2. 删除 announce-list, nodes
  3. info.private = 1
  4. info.source = `b"[{base_url}] {site_name}"`
  5. creation date += random(600, 1200) (缺失时用当前时间)
  6. **调用方需重算 info_hash**
- `summarize(p) -> dict` — JSON 摘要 (files 截断 50 条)

### 3. qb.py (344行) — qBittorrent WebUI API 客户端
基于 httpx.Client 的同步 HTTP 客户端，支持 qB 4.x/5.x。

公开 API:
- 异常: `QBError`, `QBConnectionError`, `QBAuthError`, `QBRequestError`
- `QBittorrent(host, username, password, timeout=30.0)`:
  - `login() -> bool` — POST `/api/v2/auth/login`, 兼容 qB4(Ok.)/qB5(204)
  - `add_torrent_file(path, *, savepath, category, tags, skip_checking, paused) -> dict` — multipart, **字段名 `torrents`**
  - `add_torrent_url(url, *, cookie, ...) -> dict`
  - `info(hashes=None) -> list[dict]` — hashes 支持 `|` 连接
  - `get_torrent(hashes) -> Optional[dict]`
  - `export_torrent(hash) -> bytes` — 校验返回体是合法 bencode (含 info dict)
  - `stop/start(hashes)` — qB5 用 stop/start, 404 时回退 pause/resume
  - `delete(hashes, delete_files=False)` — 默认保留数据文件
  - `set_tags/add_tags/delete_tags`, `recheck`
  - `is_completed_seeding(t) -> bool` (static) — progress==1 && completed>0 && completion_on!=-1 && state in {uploading,stalledUP,stoppedUP}

关键细节:
- 登录态过期自动重登 (401/403 时重登并重试一次)
- 写请求带 `Referer: {host}/` (CSRF)
- `_parse_add_response` 兼容 qB5 JSON 和 qB4 文本 Ok./Fails.
- 重试前回绕 multipart 文件流 (seek(0))

### 4. store.py (229行) — SQLite 去重/状态存储
单表 `relay_jobs`，info_hash 为主键。WAL 模式，threading.Lock 串行化。

公开 API:
- `STATUS_ENUM`: pending, downloaded, added_to_qb, seeded, uploaded, cross_seeded, skipped_existing, failed, skipped
- `RelayStore(db_path="data/relay.db")`:
  - `has(info_hash) -> bool`
  - `get(info_hash) -> Optional[dict]`
  - `all() -> list` (按 created_at 升序)
  - `pending_jobs(limit=50) -> list`
  - `add(job: dict) -> bool` — 新记录插入返回 True，已存在则只更新非空字段返回 False
  - `mark_status(info_hash, status, **extra)` — 校验 status 合法，只接受 _UPDATABLE 字段
  - `close()`, context manager
- Schema: info_hash TEXT PK, rss_id INTEGER, title TEXT, source_site TEXT, source_size INTEGER, qb_hash TEXT, target_status TEXT DEFAULT 'pending', target_id INTEGER, target_site TEXT, error TEXT, created_at TEXT, updated_at TEXT

### 5. config.py (582行) — 站点配置加载
纯数据层，不依赖其他项目模块。YAML/JSON + 环境变量双层配置。

公开 API:
- `ConfigError(Exception)`
- `SiteProfile` dataclass: name, role(source|target), base_url (必填), 可选 rss_url/announce_url/api_token/mteam_auth/cookie, extra dict (未知键归入)
- `RelayConfig` dataclass: sources[], targets[], poll_interval(300), keywords(["StarfallWeb","LongWeb"]), db_path, out_dir, target_announce
- `load_config(path=None) -> RelayConfig` — 默认读 config/relay.yaml → config/relay.json
  1. 读取 YAML/JSON
  2. 递归替换 `<PUT_ENV_XXX>` 占位符
  3. 按 `AUTOSEED_<SITE>_<FIELD>` 环境变量覆盖敏感字段 (env 优先于文件)
  4. 校验 (role 合法、base_url 无占位符等)
- `from_env(prefix="AUTOSEED_") -> RelayConfig` — 纯环境变量构建，无配置文件
- `save_example(path) -> Path`

关键细节:
- `_ENV_FIELD_ALIASES`: PASSKEY→passkey, COOKIE→cookie, BASE_URL→base_url, RSS_URL→rss_url, ANNOUNCE_URL→announce_url, API_TOKEN→api_token, AUTH→mteam_auth 等
- passkey 注入 rss_url: 替换 `{passkey}`/`<passkey>` 占位 + 正则替换已有 passkey= 参数
- `<PUT_ENV_XXX>` 环境变量未设置时保留占位符 → 校验时报错提示缺失变量
- 敏感字段 (api_token/mteam_auth/cookie/passkey) repr=False

### 6. titler.py (553行) — 标题结构化解析
纯函数，不联网。解析顺序很重要。

公开 API:
- `TitleComponents` dataclass: title, season, episode, year, resolution, source, medium, video_codec, audio_codec, hdr, channels, bits, group, complete(bool), raw
- `parse_title(title: str) -> TitleComponents`
- `standard_keys(c) -> dict` — 映射为标准化键 (category=tv/movie, resolution, source, medium, video_codec, audio_codec, hdr, channels)
- 常量: RESOLUTION_KEYS, SOURCE_KEYS, MEDIUM_KEYS, VIDEO_CODEC_KEYS, AUDIO_CODEC_KEYS, HDR_KEYS, CHANNEL_KEYS

解析步骤 (顺序敏感):
1. 剥离结尾制作组: `[-_.]+([A-Za-z][A-Za-z0-9._\-]{1,24})$`，排除已知组件 token (如 AAC-UBWEB → 组=UBWEB, 编码保留)
2. 提取季集 (10 个有序正则): S01E02, S01E07-S01E08, S1-S2, Season 1-2, S1(后无E), Season 1, Episode 13, EP13, 第5集, 第1季
3. 逐项剥离组件 (按序): year → resolution → hdr → video_codec → audio_codec → channels → bits → source → medium → complete → noise
4. 剩余文本 → title (_collapse: 压缩空白、去首尾 ` .-_`)

归一化映射:
- 视频: H.265/HEVC/x265 → HEVC; H.264/x264/AVC → H264
- 音频: DDP/EAC3/Dolby Digital Plus → DDP; TrueHD/AC3/DTS/DTS-HD MA/FLAC/AAC/LPCM 等
- HDR: Dolby Vision/DV/DoVi → DV; HDR10+; HDR10; HDRVIVID; HDR
- 声道: 2.0/5.1/7.1 等 + 2Audio
- 片源→(source, medium): WEB-DL→(WEB-DL,WEB), BluRay→(BluRay,BLURAY) 等

### 7. mteam_title.py (160行) — M-Team 标题规范化
公开 API:
- `MTeamTitle` dataclass: name, small_descr_cn, group, raw
- `clean_mteam_title(raw: str) -> MTeamTitle`
- `split_small_descr(raw_name) -> (name, cn)`
- 常量: CODEC_MAP (HEVC→H.265, H264→H.264), AUDIO_MAP (AAC2.0/DD5.1 紧凑写法)

算法:
1. 首个 ASCII 字母前的非 ASCII 前缀 → small_descr_cn
2. 尾部 `[-\-@]([A-Za-z0-9_.]+)$` → group
3. Token 化 (保护 WEB-DL/H.265/S01E01 等复合 token)
4. 逐 token 规范化: 编码→规范名, 音频→规范名, 年份/分辨率/片源/季集/HDR 保留
5. 空格连接 + 尾部 group

### 8. descr.py (701行) — 上传简介 HTML 构造
公开 API:
- `normalize_description(html_src) -> str` — 去 script/style, 去 logo/banner 图, 压缩空行
- `strip_source_references(html_src) -> str` — 移除"转载自/发布自/转自/来自 XX 站"引用
- `extract_sections(html_src) -> dict` — 提取 片名/中文片名/年代/国家/类别/导演/主演/剧情/IMDb/豆瓣 等
- `build_description(*, body_html, file_list, small_descr, extra_sections) -> str` — 低层组装
- `DescrBuilder` class — 顶层入口:
  - `build(item_fields: dict, parsed: dict|None) -> str` — 自动降级，绝不抛错
  - 配置: include_file_list, attach_small_descr, file_list_style(table/code), strip_references, normalize, template

### 9. detail.py (329行) — 源站详情/文件列表抓取
公开 API:
- `DetailFetchError(RuntimeError)`
- `DetailFetcher(base_url, *, cookie, referer, user_agent, timeout, follow_redirects=False)`:
  - `fetch_detail_page(torrent_id) -> str` — GET details.php?id=; 302→需要登录
  - `fetch_file_list_page(torrent_id) -> str` — GET viewfilelist.php?id=; 空 body 200→未登录
  - `fetch_file_list(torrent_id) -> list[dict]` — 解析 HTML table (class=rowfollow) 或 XML FileList
  - `fetch_all(torrent_id) -> dict` — 一次取文件列表+详情
- `parse_file_list_html(html_str) -> list[dict]` — {name, path, size, size_human}
- `parse_small_descr(html_str) -> str` — rowhead=副标题
- `parse_tags(html_str) -> list[str]` — rowhead=标签 内的 span 文本
- `parse_imdb(html_str) -> Optional[str]` — 首个 tt\d{6,}
- `parse_human_size(text) -> Optional[int]` — "6.70 GB" → 字节

### 10. source.py (522行) — 源站 RSS + 种子下载
公开 API:
- `RssItem` dataclass: id, title, link, description(全量 HTML), category_name, category_id, size, enclosure_url, guid(info_hash hex), author, pub_date, imdb, small_descr. `matches_keywords(keywords) -> bool`
- `parse_rss(xml_bytes) -> list[RssItem]` — ElementTree 解析标准 RSS 2.0
- `SourceClient(rss_url, *, timeout, headers, passkey, cookie, cf_mode="auto", qb_host/user/pass, download_mode=None, proxy)`:
  - `fetch_rss() -> list[RssItem]`
  - `download_torrent(item, out_path) -> bool`:
    - direct 模式: 后端顺序 curl_cffi → cloudscraper → httpx (缓存第一个可用的)
    - qb 模式: 交给 qB 拉种子 → export 取回 → 删 qB 临时种子
- `guid_to_infohash(guid) -> str` — 非 40 hex 则 SHA1

下载 URL 优先级: passkey+id > enclosure_url (downhash) > cookie+id

### 11. targets/__init__.py (144行) — 适配器注册 + 统一上传入口
- `_TARGET_REGISTRY = {"nexusphp": NexusPHPAPI, "nexusphp_classic": NexusPHPClassic, "mteam": MTeamAPI}`
- `upload(torrent_path, meta, cfg) -> (ok: bool, target_id: int|None, target_site: str|None)`:
  1. 解析 target 类型 (cfg["target"] 或默认 nexusphp)
  2. 构建站点实例
  3. 合并 extra 字段 (cfg + meta, meta 优先)
  4. 获取分类映射 (失败静默)
  5. 解析种子 (若 meta 无 parsed)
  6. build_upload_fields → 确保 type/category 存在
  7. site.upload_torrent → 返回结果

### 12. targets/base.py (734行) — 抽象基类 + 字段映射引擎
**最复杂的模块。** 三种适配器共享标题规范化/副标题构造/维度解析，差异仅在字段名映射。

公开 API:
- `UploadError(Exception)` — message, status_code, body_preview(截断500), existing(bool, 用于区分服务端去重)
- `TargetSite(ABC)` — name, announce_base, site_name, base_url, auth_token, passkey, timeout, _headers
  - 抽象方法: `upload_torrent(torrent_path, fields) -> dict`, `parse_fields_from_torrent(parsed) -> dict`
  - 具体方法: `build_announce()`, `client_headers()`, `make_client()`, `clean_and_dump()`, `dump_upload_fields()`
- `build_upload_fields(parsed, site, extra=None) -> dict` — **统一核心入口**
- `parse_categories_mapping(rows) -> dict[str, int]` — 递归遍历提取 {分类名: id}

**分类映射常量**:
- `MTeamCategoryID`: movie=100, tv=105, doc=404, anime=405, music=110, other=409 + 细分
- `MTeamCategoryAlias`: 中文+英文别名 → 规范 key
- `CATEGORY_FIELD_BY_TARGET`: nexusphp→"type", mteam→"category"

**维度枚举**:
- MTEAM_STANDARD: 1080p=1, 1080i=2, 720p=3, SD=5, 4K/2160p=6, 8K=7
- MTEAM_VIDEO_CODEC: H.264=1, VC-1=2, XVID=3, MPEG-2=4, H.265/HEVC=16, AV1=19, VP8/VP9=21, AVS=22
- MTEAM_AUDIO_CODEC: FLAC=1, APE=2, MP2/MP3=4, OGG=5, AAC=6, AC3/DD=8, DTS=3, DTS-HD MA=11, DDP/E-AC3=12, DDP ATMOS=13, TRUEHD=9, TRUEHD ATMOS=10, LPCM/PCM=14, WAV=15, OTHER=7
- NEXUSPHP_STANDARD: 1080p=3, 720p=2, 2160p/4K=4, SD=6
- NEXUSPHP_VIDEO_CODEC: H.264=1, H.265/HEVC=2, AV1=16, VC-1/XVID/MPEG-2=6
- NEXUSPHP_AUDIO_CODEC: AAC=14, AC3/DD=10, E-AC3/DDP=11, TRUEHD=8, DTS/DTS-HD MA/FLAC/LPCM=6
- NEXUSPHP_SOURCE: BLURAY=1, WEB=4, HDTV=5, DVDRIP=6
- NEXUSPHP_MEDIUM: BLURAY=1, WEB=4, REMUX=3, ENCODE=7, DVD=10

**三种适配器字段差异** (`_map_by_type`):
| 字段 | nexusphp (API) | nexusphp_classic (表单) | mteam |
|------|---------------|----------------------|-------|
| 标题 | name | name | name |
| 简介 | descr | descr | descr |
| 副标题 | small_descr | small_descr | smallDescr |
| IMDb | url (纯数字去tt) | url | imdb (保留tt) |
| 分类字段 | type | type | category |
| 维度 | source/medium/codec/standard/audiocodec (独立字段) | 并入 descr `[参数:...]` | standard/videoCodec/audioCodec/source/medium (独立字段) |
| 标签 | tags[] 数组 | 并入 descr `[标签:...]` | tags |
| 制作组 | team | 并入 descr | team (支持名字→ID解析) |
| 匿名 | uplver | uplver=yes | anonymous (bool, 必填) |
| 其他 | processing | — | countries/labels/mediainfo/douban |

**字段优先级**: extra (用户覆盖) > parsed 提取 > 默认值

### 13. targets/nexusphp.py (301行) — NexusPHP API 适配器
- `NexusPHPAPI(TargetSite)`: name="nexusphp", target_type="nexusphp"
- `upload_url()` → `{base_url}/api/v1/upload`
- 鉴权: `Authorization: Bearer {api_token}` (Sanctum)
- 上传: POST multipart, file 字段名 `file`, tags 字段 `tags[]`
- 必填: name, descr, type
- 去重: HTTP 409 或 body 含 torrent_existed/already exists/duplicate torrent/existed → `UploadError(existing=True)`
- 成功: `data.id` → target_id

### 14. targets/nexusphp_classic.py (215行) — 传统 NexusPHP 表单适配器
- `NexusPHPClassic(TargetSite)`: name="nexusphp-classic", target_type="nexusphp_classic"
- `upload_url()` → `{base_url}/takeupload.php`
- 鉴权: Cookie 头
- 上传: POST multipart form
- 必填: name, descr, type, cookie (缺失抛 UploadError)
- 成功判定: 302 Location 或 body 含 `details.php?id=N`
- 去重: body 含 种子已存在/already exists/重复
- boolean → "yes"/""

### 15. targets/mteam.py (377行) — M-Team API 适配器
- `MTeamAPI(TargetSite)`: name="mteam", target_type="mteam"
- `upload_url()` → `{api_base}/torrent/createOredit`
- 鉴权: `x-api-key: {auth_token}`
- 必填: name, descr, anonymous
- 去重: 10+ 个中英文关键词 (duplicate/exists/種子已存在/重复发布 等)
- 成功: `code==0` 或 `data.id` 存在
- `get_categories()` → POST `/torrent/categoryList` (解析 nameChs/nameCht/nameEng, 缓存)
- `get_teams()` → POST `/torrent/teamList` (解析 {name: id})
- announce: `https://tracker.m-team.cc/announce?credential={credential}` (服务端自行改写)

### 16. pipeline.py (368行) — 流水线编排
公开 API:
- 常量: `DEFAULT_POLL_INTERVAL=5.0`, `DEFAULT_POLL_TIMEOUT=600.0`, `TAG_PENDING="relay-pending"`, `TAG_DONE="relay-done"`
- `relay_one(item, parsed, qb, *, store, mode="A", target_announce, target_site_name, target_base_url, target_cfg, savepath, category, workdir, poll_interval, poll_timeout) -> dict`

**模式 A** (先下载做种再转):
1. store 去重 → duplicate 跳过
2. `qb.add_torrent_file(parsed.path, skip_checking=False)` → 真实下载数据
3. 轮询 `is_completed_seeding` (超时 600s → timeout)
4. `qb.export_torrent` 取回
5. `parser.clean_torrent_for_target` 清洗
6. `targets.upload` 上传
7. 目标站种子回 qB 交叉做种 (skip_checking=True, 指向源数据目录)
8. `qb.set_tags(TAG_DONE)`

**模式 B** (先转再辅):
1. store 去重
2. 清洗 → 上传 (不等下载)
3. 交叉做种
4. 标记 done

降级策略: parser/targets/store 模块缺失时降级跳过，交叉做种失败不中断，store 写入失败仅警告

### 17. CLI (scripts/relay.py 229行 + relay_run.py 337行)
统一入口 `relay.py` 分发子命令: preview/run/fetch/upload/probe/qb
- `--target`: nexusphp|mteam|nexusphp_classic
- `--download-mode`: direct|qb (默认从 AUTOSEED_DOWNLOAD_MODE)
- `--proxy`: HTTP 代理
- relay_run.py: 定时轮询主循环，每轮 RSS→筛选→下载→解析→上传→去重入库

---

## Go 重写架构建议

```
go-relay/
├── cmd/relay/main.go              # CLI 入口 (cobra)
├── internal/
│   ├── bencode/bencode.go         # bencode 编解码
│   ├── parser/parser.go           # 种子解析+清洗
│   ├── qb/qb.go                   # qBittorrent 客户端
│   ├── store/store.go             # SQLite 存储
│   ├── config/config.go           # 配置加载
│   ├── titler/titler.go           # 标题解析
│   ├── mteam/title.go             # M-Team 标题规范化
│   ├── descr/descr.go             # 简介构造
│   ├── detail/detail.go           # 详情抓取
│   ├── source/source.go           # RSS+下载
│   ├── pipeline/pipeline.go       # 流水线编排
│   └── targets/
│       ├── targets.go             # 注册+统一入口
│       ├── base.go                # 抽象+字段映射
│       ├── nexusphp.go            # NexusPHP API
│       ├── nexusphp_classic.go    # 传统表单
│       └── mteam.go               # M-Team API
├── Dockerfile                     # 多阶段构建 (FROM scratch)
├── docker-compose.yml
├── go.mod
└── go.sum
```

### 关键依赖
- `net/http` — HTTP 客户端 (替代 httpx/curl_cffi)
- `encoding/xml` — RSS 解析
- `database/sql` + `modernc.org/sqlite` — SQLite (纯 Go, 无 CGO)
- `gopkg.in/yaml.v3` — YAML 配置
- `github.com/spf13/cobra` — CLI

### 不变的设计约束
- 敏感信息只走环境变量，不落盘
- 去重键 = 源站 info_hash
- 状态机字符串不变 (SQLite 共享)
- 三种目标站适配器字段差异保留
- 分类 ID / 维度枚举 / 别名映射完整保留
- bencode key 字节序排序
- torrent 清洗: announce/private/source/creation date jitter
- 所有 HTTP 写请求带 Referer (qB CSRF)
- 管道降级: 缺模块/缺配置/交叉做种失败 均非致命
