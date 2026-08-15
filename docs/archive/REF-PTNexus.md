# PTNexus 设计参考 — AutoSeedRelay Go 重写可吸收的思路

> 分析对象：https://github.com/sqing33/PTNexus（Go 主线，`server/` 为后端）。
> 本文只谈 **AutoSeedRelay 可以吸收什么**，代码引用均以 PTNexus 仓库相对路径标注。
> 对应 AutoSeedRelay 现状：`internal/targets/*`（三适配器 + 硬编码枚举）、
> `internal/pipeline/`（RSS → qB → 清洗 → 上传）、`internal/config/`（env + SiteProfile）。

---

## 0. 项目概况与定位差异（先对齐再吸收）

PTNexus 是一个**搜索驱动的"转种迁移管理平台"**：用户在 UI 里搜索种子 → 从源站抓详情页 →
自动提取标准参数 → 补充 MediaInfo/截图 → 发布到多个目标站 → 推送下载器做种。
AutoSeedRelay 是 **RSS 驱动的"自动转种脚本"**：订阅源站 RSS → 命中关键词 → 自动清洗上传。

两者**发布层（目标站适配 / 字段映射 / 上传 HTTP）几乎完全同构**，可以直接吸收；
**获取层（源站发现）模型不同**（PTNexus 靠搜索 + 详情页 HTML，AutoSeedRelay 靠 RSS + 详情页补全），
本参考只在其交叉部分给建议。

### 最核心的一个差异：种子清洗哲学

| | PTNexus | AutoSeedRelay（现状） |
|---|---|---|
| 上传给目标站的种子 | **源站原种子原样上传**（`os.ReadFile(torrentPath)` → multipart） | `CleanTorrentForTarget` **重写 info dict** |
| 改什么 | 什么都不改 | `announce`、`announce-list`、`info.private=1`、`info.source`、`creation date` 加抖动 |
| 是否改 infohash | 否（announce 在 info dict 外） | **是**（`private`/`source` 在 info dict 内，改动即改 infohash） |
| 交叉做种 | 同一 infohash，qB 一个条目两个 tracker，自然做双站 | 新 infohash，qB 两个条目 skip_checking 指同一数据目录 |
| 目标站视角 | 看起来"同 hash 的种子" | 看起来"本站全新首发" |

PTNexus 的模型更省 qB 槽位、ratio 统计更自然，但会在目标站暴露与源站的 infohash 关联；
AutoSeedRelay 的模型更隐蔽，但要多吃一个 qB 条目。**Go 重写前应先定夺要哪种，
或者把"是否重写 info dict / 是否加 creation date 抖动"做成目标站配置项**（见 §6）。

---

## 1. 站点适配架构

### 1.1 两面适配：源站提取 + 目标站发布

PTNexus 把"站点差异"拆成**两个独立轴**，各自有引擎 + 注册表 + 回退：

- **源站提取**（`server/internal/service/acquire/extract/`）：解析源站详情页 HTML，
  产出统一的 `SeedData`。按 site code / 中文昵称路由到"特殊提取器"，否则走"公共提取器"。
- **目标站发布**（`server/internal/service/publish/`）：把统一参数翻译成目标站表单字段并上传。
  按 site code 路由到"特殊发布器"，否则走"公共发布器"。

两个轴共享同一份 `server/configs/<site>.yaml`（身兼源站标准键 + 目标站字段映射），
见 §2、§5。

### 1.2 源站提取：统一 `SeedData` + 函数注入 `Runtime`

- 统一中间结构 `SeedData`（`extract/sites/types.go`）：`Title/Subtitle/Intro/MediaInfo/
  Type/Medium/VideoCodec/AudioCodec/Resolution/Team/Source/Tags/IMDbLink/...` + 兜底 `SourceParams map[string]any`。
  所有源站无论长什么样，最后都压进这一个结构。
- 每个站一个 Go 文件（`extract/sites/hdsky.go`、`chdbits.go`、`pterclub.go`、`ssd.go`…），
  实现 `func Extract(input sites.Input, runtime sites.Runtime) (sites.SeedData, error)`。
- 通过 `Runtime` 结构体做**函数注入**：公共能力（MediaInfo 抽取、HTML→BBCode、
  标签抽取、`InferStandardizedValues`、正则表…）以函数字段塞给站点实现，
  避免 `sites` 子包反向 import 编排包造成循环依赖（`site_adapters.go` 的 `delegatedSiteExtractor` 负责桥接）。
- 引擎（`extract/engine.go`）按 `specialByCode` / `specialByNickname` 两个 map 路由，
  **特殊提取失败或结果不足时自动回退公共提取器**（`IsSSDSufficient` 这类判定）。
  注册中心 `NewDefaultEngine` 集中登记所有站点；新站 = 注册一个 extractor，不改引擎。

### 1.3 目标站发布：公共发布器 + 站点只覆写差异步骤

- 公共发布器 `PublishPublic`（`publish/publisher/public.go`）承担一切 NexusPHP 表单站：
  读 YAML 配置 → 拼 form 字段 → 落盘参数 dump → multipart 上传 → 解析详情页 / "种子已存在"。
- 站点特殊逻辑被收敛成一个**差异步骤接口**（`publish/publisher/sites/public_site.go`）：

  ```go
  type publicSitePublisher interface {
      LogModule() string                              // 日志模块名
      AttemptPrefix(input publisher.PublishInput) string
      BuildDescription(input publisher.PublishInput) string   // 重写简介
      BuildExtraFormFields(input publisher.PublishInput) (map[string]string, error)
      AdjustFormFields(input publisher.PublishInput, formFields map[string]string) // 最终字段微调
  }
  ```

- 提供 `publicSiteDefaults`（全空实现），站点**内嵌它、只覆写差异方法**——
  Go 版的"组合优于继承"。例：
  - `cbg.go`：只覆写 `AdjustFormFields`，动画/动漫按"是否有季集证据"分流到 404/405 分类；
  - `hdfans.go`：只覆写 `AdjustFormFields` 做标签/媒介细分覆盖；
  - `ptlgs.go`：只覆写 `BuildExtraFormFields`，封面与截图分两个独立字段；
  - `rousi.go`：覆写简介重建（图片源重写、BBCode/HTML/Markdown 混排解析）；
  - `zhuque.go`：完全自定义 JSON API（TNode）上传，不走公共表单。

- 路由引擎 `publish/publisher/engine/engine.go`：`switch siteCode` → 特殊发布器，default → `PublishPublic`。

### 1.4 AutoSeedRelay 可以吸收什么

1. **引入统一"中间参数结构"**。AutoSeedRelay 现在 `BuildUploadFields` 直接吃
   `*parser.ParsedTorrent` + `extra`，字段散落在三套适配器里。建议先定义一份
   `StandardParams`（type/medium/video_codec/audio_codec/resolution/team/source/tags/…），
   源站提取（RSS + 详情页）只产出这份结构，目标站适配只消费这份结构——两端彻底解耦。
2. **把"站点差异"从"整站一个实现"降维成"几个可覆写步骤"**。AutoSeedRelay 的
   `nexusphp.go / nexusphp_classic.go / mteam.go` 每个都是完整上传实现；
   可仿 `publicSitePublisher`，先写一个 `publicFormPublisher`（对应现在的 classic），
   再让新站只实现差异步骤。
3. **注册中心 + 回退**：用 `map[siteCode]factory` 路由 + 默认回退，比现在
   `targetRegistry` 线性查找更接近"平台"形态；特殊站失败时能回退公共逻辑。

---

## 2. 字段映射

### 2.1 两层映射：源标签 → 标准键 → 目标值

这是 PTNexus 最值得抄的设计。它不搞"源站标签 → 目标站标签"的**直接映射**，
而是引入**标准键中间层**（`category.movie`、`medium.bluray`、`video.h265`、`audio.dts_hd_ma`、`resolution.r2160p`、`team.hds`、`source.japan`）：

```
源站标签/下拉值 ──(source_parsers.standard_keys)──> 标准键 ──(mappings)──> 目标站表单值
   "Movies/电影"  ──────────────────────────────>  category.movie  ───────>  "401"
   "H.264/AVC"    ──────────────────────────────>  video.h264       ───────>  "1"
```

好处：**源站与目标站各自只跟标准键打交道，N 个源 × M 个目标不再需要 N×M 张映射表**。
新增一个源站 = 只配"本站标签 → 标准键"；新增一个目标站 = 只配"标准键 → 本站值"。
AutoSeedRelay 现在的 `MTEAM_STANDARD` / `NEXUSPHP_STANDARD` 等硬编码枚举，
本质是"标准键 → 目标值"表，只是散在代码里、且没有标准键这层——建议把标准键这层补上。

### 2.2 `form_fields`：语义名 → 表单字段名

每个站点 YAML 还有一张 `form_fields`（`publish/mapping/site_mapping.go` 读取），
解决"同一语义在不同站字段名不同"：

```yaml
form_fields:
  category: "type"          # 语义 category → 表单字段 type
  source: "source_sel"
  medium: "medium_sel"
  resolution: "standard_sel"
  video_codec: "codec_sel"
  audio_codec: "audiocodec_sel"
```

`ResolveBasicPublishMappings`（`publish/mapping/basic_fields.go`）把标准参数逐个
`apply(mappingKey, formKey, value, fallbackField, requireConfiguredField)`：
先查 `FormFields` 决定最终字段名，再用 `PickMappedValueWithFallback` 决定字段值。
未配置的站点字段自动跳过（`requireConfiguredField` 控制是否强制）。

### 2.3 降级链（fallback chains）

`publish/mapping/fallback.go` 支持**降级链**：标准键映射不到时，按 `global_mappings.yaml`
里 `fallback_chains` 指定的顺序逐个试。例如 `audio.truehd_atmos` 目标站没有，
就依次试 `audio.truehd` → `audio.other`；`category.mv` 可退 `category.music` → `category.other`
（见 `configs/global_mappings.yaml` 1151 行起）。这比 AutoSeedRelay
现在的"精确匹配 → default"多一层"近义回退"，对画质/音轨这种细粒度维度很实用。

### 2.4 反向映射

`server/internal/service/reversemapping/reverse_mappings.go` 从标准键字典反向构建
"标准键 → 展示名"，用于 UI 下拉。对 CLI 来说，反向映射 = 给 `relay preview` 输出
"这个标准键会变成目标站的哪个值"的可读提示，成本很低、收益直观。

### 2.5 AutoSeedRelay 可以吸收什么

1. **补上"标准键"中间层**，把散落的枚举改成 YAML 驱动的两段映射：
   `title/详情页 → 标准键` + `标准键 → 目标站值`。
2. **字段名与字段值分开配**：`form_fields`（名字）+ `mappings`（值），
   语义字段（type/medium/codec/standard…）统一，各站只填差异。
3. **加 `default` 兜底 + 降级链**：mapping 里留 `"default": "<值>"`，缺失时兜底；
   画质/音轨类可配置降级链。
4. **反向映射给 preview 用**：`relay preview --target` 时打印"标准键 → 目标值"对照，
   一眼看出会不会丢维度。

---

## 3. 标签/分类映射

### 3.1 全局标准键字典 `global_mappings.yaml`（1260 行）

按 `type / medium / video_codec / audio_codec / resolution / year / team / source / tag`
分节，把**所有常见站的原文文案**归一成标准键，一网打尽：

```yaml
global_standard_keys:
  type:
    "电影": "category.movie"
    "Movies/电影": "category.movie"
    "TV Series": "category.tv_series"
    "Documentaries": "category.documentaries"
    ...
  audio_codec:
    "DTS-HD MA": "audio.dts_hd_ma"
    "TrueHD Atmos": "audio.truehd_atmos"
    ...
```

这份字典是"社区维护的常识库"：**新站 90% 的标签在字典里已经映射过了**，
剩下 10% 才需要写进站点 YAML 的 `source_parsers.standard_keys` 覆盖/补充。

### 3.2 站点级 + 全局两级查表，先精确后部分

`processing/tagging/mapping.go` 的 `MapTagsToStandard`：

1. 已经是 `tag.xxx` 形式的直接透传；
2. 先查**站点级** `standard_keys.tag`（精确），再查**全局** `global_standard_keys.tag`；
3. 两者都支持**部分/子串匹配**，且按"源串长度降序"排序（越长越优先，避免 `HDR` 吞掉 `HDR10+`）；
4. 映射不到的标签单独收集返回（`unmapped`），供日志/告警，**不静默丢弃也不强塞**。

### 3.3 归属型标签与转种限制

- `DetectRestrictedTags`（`publish/uploader/helpers.go`）+ `processing/tagging/restricted.go`：
  检测 **禁转 / 限转 / 分集** 标签，命中即拦截，不发布。这与 AutoSeedRelay 的
  "归属型标签（官方/独占/首发）跨站应 skip"（`docs/TAG-MAPPING.md` §1.2）是同一类问题，
  PTNexus 把它做成了**发布前硬拦截**而不是映射丢弃。
- 站点 YAML 里 `tag` 段可用 `"default": null` 显式表示"该标签在此站没有标准键"
  （如 hdsky.yaml 的 `"default": null`），避免误映射。

### 3.4 genre 推导

`genre_options_by_type`（如 rousi.yaml）：按 type 列出该站可选的 genre 选项，
从 tags 取交集推导 `attributes.genre`。AutoSeedRelay 若想给目标站补"类型"字段，可借鉴。

### 3.5 AutoSeedRelay 可以吸收什么

1. **从"源标签 → 目标标签"改成"源标签 → 标准 tag.* 键 → 目标标签 ID"**。
   AutoSeedRelay 现在 `tag_mapping.json` 是 `{"国语": {"dev": {...}, "luckpt": {...}, "mteam": {...}}}`
   —— 直接映射在站点一多就会爆炸；标准键层可复用。
2. **部分匹配 + 最长优先**：中文标签常有变体（`简中`/`简繁中字`/`中字`），
   子串匹配能省掉大量手工映射。
3. **`unmapped` 不静默丢弃**：把没映射上的标签打日志，`preview` 里能看到，
   避免"标签悄悄没了"。
4. **禁转/限转/分集做成发布前拦截**，而不是映射丢弃——这是合规底线。

---

## 4. 上传流程

### 4.1 三层：workflow / publisher / uploader

- **workflow**（`publish/workflow/`）：编排整条迁移（构建 Context、解析目标、调发布、写日志、回写状态），
  与 AutoSeedRelay 的 `pipeline.go` 角色相同。
- **publisher**（`publish/publisher/`）：路由 + 字段组装（§1.3）。
- **uploader**（`publish/uploader/`）：**纯 HTTP 上传器** `TryUploadTorrent`（`upload_http.go`），
  不关心站点语义，只收 `uploadURL/baseURL/cookie/fileField/torrentFile/formFields`。

### 4.2 上传响应解析：已存在检测（直接可抄）

`TryUploadTorrent` 对目标站响应做了**多信号判重**，AutoSeedRelay 的 `UploadError.Existing`
目前只判简单情况，可补齐：

1. 重定向 `Location` 里带 `existed=1` / `exist=1` → 已存在；
2. 响应正文含 `已存在` / `already exists` → 已存在（并尝试从中抠详情页 URL）；
3. `<title>` 解析 + `looksLikeUploadFailurePage` / `looksLikeUploadFormPage`
   （返回上传表单页 = 字段缺失/校验不过/会话失效，而不是成功）；
4. `Location` 是 `login.php` → 判定会话过期；
5. 网络错误按可重试性分类（`ShouldRetryUploadNetworkError`），指数退避重试 3 次；
6. 成功时从 `Location` 或正文正则抠 `details.php?id=N` / `torrent/<uuid>` 详情页 URL。

### 4.3 站点差异封装

特殊站通过 `AdjustFormFields`（最终字段整体后处理）和 `BuildExtraFormFields`（追加字段）
完成差异化，公共上传底座不动（§1.3）。这对 AutoSeedRelay 的价值是：
**绝大多数新站只需一个 YAML + 最多一个 `AdjustFormFields`，不用写整站上传器。**

### 4.4 AutoSeedRelay 可以吸收什么

1. **把上传 HTTP 抽成独立 `uploader` 层**，与字段组装解耦；三套适配器共用一个
   multipart 上传 + 响应解析器。
2. **升级"已存在"检测**为多信号（Location 参数 / 正文关键词 / 返回表单页 / 登录跳转），
   `skipped_existing` 判定会更准，避免误报 or 漏报。
3. **网络重试 + 退避**下沉到 uploader，按错误类型判断是否可重试。
4. **详情页 URL 解析**标准化（Location 优先 → 正文正则兜底），供去重入库。

---

## 5. 配置管理

### 5.1 结构配置 vs 凭据分离

PTNexus 把"会变但可公开"的映射/结构（`server/configs/<site>.yaml`，版本化提交）与
"用户私密"的凭据（DB `sites` 表里的 cookie/passkey）分开：

- 站点结构配置：YAML，随代码进 git，`LoadSitePublishConfig` 读 + `sync.Map` 缓存。
- 凭据：DB，用户在 UI 维护；`SyncSitesFromJSON`（`repository/sites_sync.go`）从
  `sites_data.json` 同步站点元数据时**明确保护 cookie/passkey 不被覆盖**。
- 全局配置：`server/data/config.json`，读时与默认值深合并（`config/manager.go`），
  只存非默认项；密码类敏感字段 `Save` 时清空回写。

### 5.2 一个站点一个 YAML，身兼提取 + 发布

同一份 `configs/hdsky.yaml` 既有**源站侧** `source_parsers.standard_keys`（本站文案 → 标准键），
也有**目标站侧** `form_fields` + `mappings`（标准键 → 本站表单值）+ `anonymous` 配置。
AutoSeedRelay 现在 source 与 target 的 SiteProfile 是同一结构，方向一致；
可以更进一步：**每个站点 = 一个 YAML，内含标准键映射表（双向），不分 source/target 两套**。

### 5.3 站点身份字段

`sites` 表用 `nickname`（中文昵称，如"天空"）作主展示名，`site`（site code，如 `hdsky`）作
代码标识，`base_url` 解析核心域名用于"详情页 URL 反查站点"（`fetch/site_lookup.go`，
支持站点换域名后的 core-domain 兜底匹配）。AutoSeedRelay 的关键词/去重目前不依赖站点身份，
但"详情页反查站点"这招对多源站去重很有用。

### 5.4 AutoSeedRelay 可以吸收什么

1. **把映射表从代码枚举挪进版本化 YAML**（`config/sites/<site>.yaml`），
   凭据继续走环境变量/密钥文件——保持"结构进 git，秘密不进 git"。
2. **每站点一份 YAML，双向映射**（源站文案 → 标准键 + 标准键 → 目标站值），
   替代现在 `MTEAM_*` / `NEXUSPHP_*` 硬编码和零散的 `tag_mapping.json`。
3. **配置读时深合并默认值 + 缓存**，减少重复解析（YAML 解析一次、缓存命中）。
4. **站点 code 与展示名分离**，并支持"core-domain 反查站点"，为将来多源站去重打底。

---

## 6. 种子清洗

（这是两个项目差别最大的地方，重写前必须决策，详见 §0。）

### 6.1 PTNexus：上传原种子，动都不动

- 下载源站 .torrent → 交下载器做种 → 上传时 `os.ReadFile` 原样 multipart 提交。
- announce 在 info dict **外**，目标站接管后会自己换成自己的 tracker，infohash 不变。
- 优点：qB 一个条目、同一 hash 双 tracker、天然交叉做种；实现极简。
- 缺点：目标站能看到与源站相同的 infohash，存在"被识别为搬运"的风险。

### 6.2 AutoSeedRelay 现状：重写 info dict（等价于"换一个 infohash"）

- `CleanTorrentForTarget`（`internal/parser/parser.go`）改：
  `announce`、删 `announce-list`/`nodes`、`info.private=1`、`info.source="[base_url] site_name"`、
  `creation date += 600~1200s 随机抖动`。
- `info.private` / `info.source` 在 info dict **内**，改动即改 infohash——
  因此交叉做种靠 `CrossSeedInQB` 把**清洗后的目标站种子**再挂回 qB（skip_checking），
  qB 里变成源站 + 目标站两个条目指向同一数据目录。
- 优点：目标站视角是"全新首发"，无 infohash 关联；`creation date` 抖动进一步去指纹。
- 缺点：多占一个 qB 条目；若源站已 `private=1` 而目标站又要求 `private=1`，
  把 `source` 改成目标站值仍会改 infohash——这是**有意为之**，不是 bug。

### 6.3 Go 重写建议

1. **把清洗策略做成配置**，不要写死：每个目标站可选
   `clean_mode: none | rewrite`（none = PTNexus 原样上传；rewrite = 现状重写）。
2. **明确 infohash 是否可变**：`none` 时 infohash 不变 → qB 交叉做种走"同一 torrent 补 tracker"；
   `rewrite` 时 infohash 变 → 才需要 `CrossSeedInQB` 第二条目。两者上传后流程不同，逻辑要分叉。
3. `creation date` 抖动、`source` 字段格式（`[base_url] site_name`）作为可配参数，
   不同站对"搬运痕迹"敏感度不同。
4. 若选 `none`，下载器/去重键仍用源站 infohash；若选 `rewrite`，注意目标站返回的
   target_id / 目标站 infohash 与源站 infohash 不再是同一个，去重记录要区分。

---

## 7. 其它值得吸收的工程细节

1. **函数注入避免循环依赖**：`Runtime` 结构体存函数字段（`sites.Runtime`），
   `delegatedSiteExtractor` 做适配层。Go 里跨包复用公共能力且不互相 import 时可用这招。
2. **`UPLOAD_TEST_MODE` 测试开关**：`PublishPublic` 里 `UPLOAD_TEST_MODE=true` 跳过真实上传，
   返回模拟详情页。AutoSeedRelay 已有 `--dry-run`，可对照补一条"假成功"路径跑全流程。
3. **上传参数落盘 dump**（`publish/uploader/params_dump.go`）：上传前把 formFields + 上下文
   写 `data/tmp/`，出问题可复现，不用抓包。
4. **多候选下载链接**（`acquire/fetch/torrent_fetch.go`）：详情页里正则抽 `download.php?` /
   `/api/torrent/.../download/` / `.torrent` 直链，+ 按 passkey 构造直链，全部试到成功；
   下载字节先 `isLikelyTorrent` 校验再落盘。AutoSeedRelay 的 RSS enclosure 直链可扩展成多候选。
5. **配置缓存统一 `sync.Map` + 路径解析**：`config.ResolveRuntimePaths()` 一处定路径，
   `LoadSitePublishConfig`/`loadGlobalTagMappingTable` 等都走缓存，避免反复读盘。
6. **日志分层 + 每步 AppendLog**：PTNexus 的发布流程把"尝试记录"逐行拼进 `AttemptDetailLog`，
   UI/CLI 都能拿到完整过程——AutoSeedRelay 的 `preview` 可以照抄这种"可回放日志"。

---

## 附：文件索引（对照查阅）

| 主题 | PTNexus 参考文件 | AutoSeedRelay 对应 |
|---|---|---|
| 源站提取引擎/适配 | `server/internal/service/acquire/extract/{engine.go,site_adapters.go,sites/*.go}` | `internal/detail/`, `internal/source/` |
| 统一中间结构 | `server/internal/service/acquire/extract/sites/types.go` | `internal/parser/` `ParsedTorrent` |
| 目标站发布引擎 | `server/internal/service/publish/publisher/{engine/engine.go,public.go,sites/*.go}` | `internal/targets/` |
| 差异步骤接口 | `server/internal/service/publish/publisher/sites/public_site.go` | `internal/targets/targets.go` `TargetSite` |
| 字段映射 | `server/internal/service/publish/mapping/{site_mapping.go,basic_fields.go,fallback.go}` | `internal/targets/base.go` `mapByType` |
| 标准键字典 | `server/configs/global_mappings.yaml` | `docs/TAG-MAPPING.md` |
| 站点 YAML 样例 | `server/configs/{hdsky,luckpt,ptlgs,rousi}.yaml` | `config/relay.yaml` SiteProfile |
| 标签映射逻辑 | `server/internal/service/processing/tagging/mapping.go` | `internal/targets` + `tag_mapping.json` |
| 上传 HTTP | `server/internal/service/publish/uploader/upload_http.go` | `internal/targets/base.go` `postMultipart` |
| 已存在检测 | `server/internal/service/publish/uploader/upload_http.go` | `internal/targets/base.go` `UploadError.Existing` |
| 配置管理 | `server/internal/config/{manager.go}` + `server/configs/` | `internal/config/config.go` |
| 站点凭据/同步 | `server/internal/repository/{site_repository.go,sites_sync.go}` | 环境变量 + SiteProfile |
| 反向映射 | `server/internal/service/reversemapping/reverse_mappings.go` | 无（可新增） |
