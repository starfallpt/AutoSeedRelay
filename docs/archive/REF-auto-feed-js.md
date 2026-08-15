# auto_feed_js 参考分析 — AutoSeedRelay 可吸收的设计

> 分析对象：<https://github.com/tomorrow505/auto_feed_js>（核心文件 `auto_feed.user.js`，约 3 万行）。
> 性质：浏览器用户脚本（油猴）。在源站**详情页**点"转发"链接 → 跳到目标站上传页 → 脚本自动填表（标题/简介/分类/维度/种子文件）→ 人审后手动提交。
> 与 AutoSeedRelay 的定位差异：它是**半自动、浏览器内、站点适配硬编码**；AutoSeedRelay 是**全自动、服务端 Go 守护进程、适配器可配置**。但它的核心数据流（源站 → 归一化中间模型 → 目标站映射）与 AutoSeedRelay 完全同构，本文件提炼其可复用的设计。

---

## 目录

1. [总体架构与可复用核心](#1-总体架构与可复用核心)
2. [种子清洗逻辑（改 announce / 强制 private / 加 source）](#2-种子清洗逻辑)
3. [站点适配（多站点差异管理）](#3-站点适配)
4. [简介 / 描述处理（含图片处理）](#4-简介--描述处理)
5. [标签 / 分类映射](#5-标签--分类映射)
6. [配置方式](#6-配置方式)
7. [去重机制](#7-去重机制)
8. [AutoSeedRelay 可直接吸收清单（汇总）](#8-autoseedrelay-可直接吸收清单汇总)

---

## 1. 总体架构与可复用核心

脚本的关键在于一个**归一化中间模型** `raw_info`（约 `auto_feed.user.js:1594` 起定义），所有源站差异都被收敛到这一组语义字段上，再按目标站映射提交：

```
源站详情页 DOM
   ↓ walkDOM() 按源站规则转 BBCode
raw_info {
  name, small_descr, descr, full_mediainfo,
  url (imdb), dburl (douban), bgmurl, torrent_url,
  type (电影/剧集/动漫/纪录/综艺/音乐/体育/MV/游戏/书籍),
  source_sel (地区), medium_sel (媒介), codec_sel, audiocodec_sel, standard_sel (分辨率),
  origin_site, origin_url, edition_info, ...
}
   ↓ dictToString() 编码进转发 URL (#separator# + base64)
   ↓ 目标站上传页 stringToDict() 还原 → fill_torrent()/选分类/填表单
```

**AutoSeedRelay 可吸收的核心**：这套"源站差异 → 语义字段 → 目标站差异"的两级收敛模型，与现有 `docs/SITE-MAPPING-GUIDE.md`、`internal/targets/*` 的标准化键体系方向一致。auto_feed_js 多做了两件事值得补：
- 语义字段的**值域也是归一化的枚举**（`type` 是 电影/剧集/动漫/纪录/综艺…，而不是各站的数字 ID）；
- 源站解析与目标站填表**完全解耦**：解析端只产出语义字段，目标端只消费语义字段。AutoSeedRelay 的适配器目前把"解析 + 映射"耦合在一起，可以往这个方向拆。

---

## 2. 种子清洗逻辑

核心函数 `build_blob_from_torrent()`（`auto_feed.user.js:4743`）。它对下载到的 .torrent **做了字符串级 bencode 手术**，而不是用 bencode 库。

### 2.1 清洗步骤（按执行顺序）

| 步骤 | 实现 | 说明 |
|---|---|---|
| 前置防护 | `r.match(/value="firsttime"/)` | 源站要求"必须先下载过一次"才允许下载种子，否则弹窗中止 |
| 前置防护 | `r.match(/Request frequency limit/)` | 目标站 TTG 限频提示，中止 |
| **改 announce** | `8:announce<N>:...` 整体替换为 `8:announce<新len>:<forward_announce>` | 目标站 announce 来自跳转参数；**缺省 fallback 是 `https://hudbt.hust.edu.cn/announce.php`**（脚本作者自己的站） |
| **creation date 抖动** | 解析 `13:creation datei<N>e`，`新值 = 旧值 + 600 + rand(600)` | 让种子看起来像"刚生成"，避免被识别为直接搬运的副本 |
| 补 encoding | 追加 `8:encoding5:UTF-8` | 统一编码 |
| **强制 private** | 正则取 `4:info[\s\S]*?privatei\de`，把 `privatei0e` 替换为 `privatei1e` | 只改 0→1，字节数不变，info 的 `4:info<len>:` 前缀仍然有效 |
| **加 source** | 追加 `6:source<len>:<FORWARD_SITE 大写>` | **加在 info 字典之外**（种子字典的顶层 key），不改变 infohash |

### 2.2 边界处理与缺陷（Go 版要注意）

1. **`source` 放在 info 外**：`d8:announce…13:creation date…8:encoding…4:info…6:source…ee`，source 是顶层 key，**不参与 infohash**。若源种子已经是 private=1（PT 站下载的种子基本都是），则清洗后 infohash 与源种子**完全相同**，仅在目标站内部做 infohash 去重时才有意义。主流转种工具（如 cross-seed 思路）通常把 `source` 放进 **info 内**，让每个站的副本有独立 infohash。建议 Go 版做成可配置：默认放 info 内（得到新 infohash，避免跨站同名资源被目标站按 infohash 判重），并允许目标站偏好顶层。
2. **字符串正则的脆弱性**：
   - 正则 `4:info[\s\S]*?privatei\de` **要求 info 内已有 private 键**。如果某个种子没有 private 标志（公共种子），整个 info 片段匹配不上 → 产物损坏。Go 版用 bencode 库解析后**不存在该键则新增**，天然规避。
   - 非贪婪 `[\s\S]*?` 匹配到**第一个** `privatei\de`，若 info 内文件名恰含 `privatei0e` 字样会提前截断。
   - 所有长度前缀（`8:announce12:...`）靠手算，改一处忘一处就损坏。Go 版用 `github.com/anacrolix/torrent/bencode` 或 `jackpal/bencode-go` 重新序列化即可。
3. **announce 缺失即中止**：`alert('种子文件加载失败！！！')`。Go 版应为"目标站必须配置 announce_url，缺失则跳过/告警"。
4. **禁止转种名单**（`8:announce\d+:.*(please.passthepopcorn.me|blutopia.cc|beyond-hd.me|eiga.moi|hd-olimpo.club|secret-cinema.pw|monikadesign.uk)`）：对来自这些站的种子，脚本仍会重建 torrent，但只提取 name 填标题框（这些站通常禁止跨站转载）。Go 版可把这类"禁止转发源站"做成配置黑名单。

### 2.3 源种子的获取方式

- 源站详情页解析 `torrent_url`：`a[href*="download.php"]:contains(torrent)` 或 `#download_link` 的 value 等（`auto_feed.user.js:9423-9449`），并针对约 20 个站点写了不同的选择器。
- 还支持把种子/图片直接推到 **qBittorrent / transmission**（`download_to_server_by_file`，`auto_feed.user.js:3781`）：qb 走 `/auth/login` + `/torrents/add`，可带 `savepath`、`category`、`skip_checking`，且支持**按源站设置上传限速**（`siteUpLimits`：CMCT=128MB、Audiences=131MB）。这与 AutoSeedRelay 的 qB 做种链路（RSS → qB → 取回种子）互补：auto_feed_js 是"源种子下到本地做种"，Go 版是"源种子交给 qB 做种再取回"。

> 总结：Go 版应**保留"改 announce + 强制 private + 加 source + creation date 抖动 + 补 encoding"这一清洗集合**，但用真正的 bencode 库重写，并把 source 位置、禁止转发名单、缺省 announce 都做成配置项。

---

## 3. 站点适配

### 3.1 两级站点注册表

- `default_site_info`（`auto_feed.user.js:1020-1129`）：**约 100 个目标站**，形如 `'MTeam': {'url': 'https://kp.m-team.cc/', 'enable': 1}`。
- `o_site_info`（`auto_feed.user.js:1358`）：**约 60 个特殊源站**（国内站 + 国外站），只存 URL。
- `find_origin_site(url)`（`:1898`）：用域名正则匹配两级注册表，识别当前页面属于哪个源站；匹配不到返回 `'other'`。

**可吸收点**：AutoSeedRelay 的 `internal/config/config.go` 已有 per-site YAML。可以加一个"内置站点注册表"（name + base_url + 是否启用），用户只需在 YAML 里 `enable: false` 关闭站点，而不是删除配置。同时保留"域名变体自动纠正"的思路（见 §6）。

### 3.2 差异怎么管理

auto_feed_js 把站点差异**硬编码成 if/else 链**，分布在五个层面：

| 差异层面 | 函数 / 位置 | 例子 |
|---|---|---|
| 页面角色判定 | `judge_if_the_site_as_source()`（`:2237`） | 按 URL 返回 mode：0=上传页、1=详情页、2/4/5/6/7=特殊站点 |
| 源站 DOM 解析 | `walkDOM()`（`:1982`）及其外围 | 每种节点（FONT/A/TABLE/IMG/SPOILER…）带站点正则分支 |
| 国内 vs 国外 | `judge_if_the_site_in_domestic()`（`:2358`） | 国内 NexusPHP 系结构一致、免查豆瓣；国外站结构各异 |
| 目标站文件框选择器 | `fill_torrent()`（`:4821`） | `#torrent` / `input[name=file_input]` / `input[name=torrent_file]` / `#torrent-input` / `#form_item_torrent` / `ant_form_instance` 等 |
| 目标站跳转 URL / 分类 | `set_jump_href()`（`:4238`） | 每站一个上传页 URL + `category_id` 映射 |

**可吸收点**：这套"源站解析规则 + 目标站填表规则"以**规则表**（YAML）形式下沉，比代码 if/else 更可维护——正是 AutoSeedRelay 适配器的方向。auto_feed_js 里的实用判定技巧可以直接借：
- **用"有没有 `/api/v1/sections`"区分 NexusPHP 新 API 站**（AutoSeedRelay 的 `probe` 已实现）。
- **按站点归属地（国内/国外）批量套用默认行为**：国内站可共用一套 walkDOM 默认规则，国外站单独配。Go 版可给适配器加一个 `region: domestic|foreign` 属性来批量决定默认清洗规则。

### 3.3 现代 SPA 站的处理

`selectDropdownOption()`（`:4967`）针对 Ant Design / React 下拉（YemaPT、ZHUQUE 等）做：mousedown 展开 → 轮询 `.rc-virtual-list-holder` 出现 → 按 `title` 匹配选项点击 → 滚动加载更多再试。这是浏览器 UI 自动化特有逻辑，**Go 服务端用不到**，但说明了一个原则：**同一站点的 API 与表单、SPA 与 PHP 页面差异巨大，判定依据永远是"探测结果"而非站名**（与 `docs/SITE-ADAPTER.md` §1.1 一致）。

---

## 4. 简介 / 描述处理

### 4.1 源页 DOM → BBCode（walkDOM）

`walkDOM()`（`:1982`）对详情页 DOM 做**前序遍历递归**，按节点类型和源站正则转换成 BBCode：

- `FONT` → `[color=#xxx]` / `[size=n]` / `[font=face]`；`SPAN` 带颜色 → `[color]`；`U` → `[u]`；`B` → `[b]`。
- `A` → `[url=href]text[/url]`（CHDBits 跳过 `pic/hdl.gif`）。
- `IMG` → `[img]src[/img]`；**懒加载站（TJUPT）取 `data-src` 而不是 `src`**。
- `TABLE` → 有的站（TTG/bwtorrents）包 `[quote]`，有的站（U2）清空，NexusHD 的 `mediainfotabletable` 清空。
- `BLOCKQUOTE/FIELDSET` → `[quote]`，且**命中免责声明关键词块直接清空**：`(温馨提示|郑重声明|您的保种|商业盈利|相关推荐|自动发布|仅供测试宽带|不用保种|本站仅负责连接|感谢发布者|转载请注意礼节)`。
- `SPOILER`（NexusHD）→ `[quote=标题]内容[/quote]`；`codemain`（PTer/Audiences 等）→ 包 `[quote]` 或拆出 MediaInfo。
- 换行处理：`BR` 在部分站转 `\r\n`，其余忽略。

**可吸收点**：这是一套**可配置的 HTML→BBCode 规则集**。Go 版 `internal/descr/descr.go` 已做清洗，可以补：免责声明关键词块剥离、懒加载 `data-src` 优先、`[quote=标题]` spoiler 展开。建议把规则做成"默认规则 + 站点覆盖"（如 TTG 的 TABLE→quote）。

### 4.2 归一化清洗（fill_raw_info）

`fill_raw_info()`（`:3127`）对 descr 做统一清洗：
- URL 解码：`%3A→:`、`%2F→/`；删除空 `[quote][/quote]`、`[b][/b]`；折叠多空行 `\n\n+ → \n\n`。
- 硬编码替换特定图床防盗链图（`pic.imgdb.cn` → `pixhost`）。
- 删除 `引用...` 行、`ARDTU` 行。
- **从 descr 反推 imdb/douban 链接**（缺 `raw_info.url` 时正则抓 `imdb.com/title/tt\d+`）。
- 目标站特化：LaJiDui 追加 `[quote]转载种子来源：…[/quote]`；OurBits 修 `[quote]\n`。

### 4.3 副标题推导（get_small_descr_from_descr）

`:2744`：无 small_descr 时从 descr 抓 `译名/片名`（`◎译 名[:：]...`、`◎片 名[:：]...`），再按标题里的 `S\d{2}E\d{2}` 拼 `*第N季 第N集*`，或 `◎集 数` 拼 `全N集`，最后拼 `类别：…`。**可吸收点**：Go 版可对 `small_descr` 做同样的"标题季集数 + descr 类别"推导。

### 4.4 类型重分类（descr → type）

`:3149`：若 `type == '电影'` 且 descr 命中 `类...别...纪录片` → `纪录`；命中 `动画` → `动漫`。`after_douban()`（`:4659`）里也有一份。**可吸收点**：分类映射前做"基于简介的类型纠偏"，能显著提升跨站分类准确率（尤其是"纪录片/动漫"这两个常被标题误导的类型）。

### 4.5 制作组感谢（add_thanks）

`:1971` + `reg_team_name`（`:1928`）：一张 **"组名正则 → 站点"表**（MTeam、CMCT、CHDBits、WiKi、TTG…约 40 条），标题命中则 descr 顶部加：
```
[quote][b][color=blue]{站点}官组作品，感谢原制作者发布。[/color][/b][/quote]
```
**可吸收点**：Go 版可在 `descr` 前拼接"转载来源/致谢"块，规则用 YAML 存（正则 + 站点名 + 致谢模板）。某些目标站（如 LaJiDui）要求注明转载来源，正好复用这套表。

### 4.6 MediaInfo

- `simplifyMI()`（`:7916`）：把完整 MediaInfo 压缩成 **General / Video / Audio / Text 摘要**（`get_general_info`/`get_video_info`/`get_audio_info`/`get_text_info`）。已含 `QUICK SUMMARY` 则原样返回；BD 的 `Disc INFO` 走 `full_bdinfo2summary`；HDT 站不简化。
- 摘要格式是"键对齐点行"（`Release Name.......: xxx`），符合大多数 PT 站简介审美。
- **可吸收点**：AutoSeedRelay 目前把 mediainfo 当纯文本透传（`docs/RELAY-FIELDS.md`）。可加一个可选的"简化 MI"步骤，并做成 per-target 开关（有的站要求完整 MI，如 HDT）。

### 4.7 图片处理（重点）

| 能力 | 函数 | 说明 |
|---|---|---|
| 图床转存 | `rehost_single_img()`（`:4674`） | 支持 catbox / imgbb / gifyu / freeimage；POST 原图 URL，把返回拼成 `[img]` 或"缩略图+原图"BBCode |
| 取原图 | `get_full_size_picture_urls()`（`:2917`） | 把图床**缩略图 URL 改写回原图**：imgbox `thumbs2→images2`、`t.png→o.png`；pixhost `//t→//img`、`thumbs→images`；pter/ttg/瓷器/img4k `th.png→png`、`md.png→png`；beyondhd `th.png→png`、`/t/→/i/`；ttg `_thumb.png→.png` |
| 海报抓取 | `getImage`/`b64toBlob`/`uploadToPtpimg` | 抓 IMDb/豆瓣海报转存 ptpimg 等 |
| 350px 缩略 | `deal_img_350()`（`:2199`） | 生成宽 350 的缩略 BBCode（部分站限制截图宽度） |
| 批量截图 | 设置面板 | "从第 N 张开始每隔 M 张取第 K 张"，站点头像相册自动抓图 |

**可吸收点**（对 Go 版性价比最高的三件）：
1. **缩略图→原图 URL 改写表**：纯字符串替换、零请求成本，直接搬进 `internal/descr`。
2. **catbox 转存**：单接口 `POST https://catbox.moe/user/api.php`（`url=...&reqtype=urlupload`），无需注册 token，非常适合做默认图床。
3. **descr 中的图片提取/替换管线**：`[img]...[/img]`/`[url=...][img]...[/img][/url]` 的提取、删除、批量替换，Go 版可用一个小的 BBCode 图片提取器实现（`get_full_size_picture_urls` 的 `[url=…][img]…[/img][/url]` 正则可照抄）。

---

## 5. 标签 / 分类映射

### 5.1 归一化 type 枚举

所有源站最终收敛到中文枚举：`电影 / 剧集 / 动漫 / 纪录 / 综艺 / 音乐 / 体育 / MV / 游戏 / 书籍 / 其他`（`type_dict`，`:4287`）。

### 5.2 分类映射 = "归一化 type → 目标站 category"

`set_jump_href()`（`:4238`）按目标站把 type 映射到上传页的 `category_id`/`type` 参数：

- Aither/OnlyEncodes/DarkLand/ReelFliX/DesiTorrents：`{'电影':1,'剧集':2,'动漫':2,'综艺':2,'纪录':2,'音乐':3,'体育':2,'MV':3}`，且**剧集判断加边界**——`纪录` 若标题不含 `S\d+|E\d+`（单集纪录片）→ category 1。
- BYR：每类对应不同 `upload.php?type=N`（电影=408、剧集=401、综艺=405、音乐=402、动漫=404、纪录=410）。
- ACM/BLU/Tik：剧集/纪录/综艺 → category 2，电影 → category 1。
- avz/PHD/CNZ（电影/剧集站）：只分 movie/tv 两条上传路由。
- 兜底：不认识的 type → `category_id=1`。

**可吸收点**：这套"归一化 type + 每站 type_dict + 单集/多集边界修正 + 兜底"逻辑，与 AutoSeedRelay 的 `ResolveCategoryID` + `tags_map` 完全同构。auto_feed_js 多了两个值得抄的细节：
1. **纪录片/动漫/综艺的"是否剧集"边界**（标题 `S\d+|E\d+` 判断），这直接影响单集电影归类到"电影"还是"剧集"。
2. **type_dict 以"站点家族"共享**（Aither 系 5 个站同一张表），Go 版可做成"适配器模板 + 覆盖"。

### 5.3 标签与维度

- 标签：auto_feed_js 主要靠目标站上传页的**复选框/下拉自动勾选**（`check_label`，`:3343`）与"并入 descr"两种策略。AutoSeedRelay 的 `tags_map`（中文标签名 → 数字 ID）思路更优，继续沿用。
- 维度：`raw_info.codec_sel/standard_sel/audiocodec_sel/medium_sel/source_sel` 通过"**标题 → descr → MediaInfo**"三级正则推断（`fill_raw_info` 里的 `name.codec_sel()`、descr 的 `Writing library.*x265`、`Height...pixels` 等），推断顺序是**先标题、再 descr、最后 MediaInfo**。Go 版 `internal/titler` 已做标题解析，可补上"descr/MediaInfo 兜底"两级的正则集。

---

## 6. 配置方式

### 6.1 浏览器场景下的"配置"

- 没有集中配置文件，全部存 `GM_getValue`（浏览器本地存储）：`used_site_info`（每站 url + enable）、`site_order`（站点显示顺序）、`used_rehost_img_info`（图床 API key）、`used_search_list`、`used_common_sites`、`used_signin_sites`、`extra_settings`。
- 设置 UI 就是源站详情页里的一张配置表：转发站点复选框（可拖拽排序）、常用站点、图床、TorrentLeech RSS key 等。
- **域名变体自动纠正**：按当前页面 URL 修正站点配置里的域名——`tjupt.org`/`open.cd`/`pthome.net` 的 www 变体；CHDBits 备份域名按地区码；NexusHD v6；MTeam 的 `kp.m-team.cc`/`zp.m-team.io`；BTN 的 `backup.landof.tv` 等（`:1201-1245`）。

### 6.2 跨页面状态传递

`dictToString`（`:2406`）把 `raw_info` 序列化为 `key + '#linkstr#' + value` 交错串 → `btoa(encodeURIComponent(...))`，追加在目标站跳转 URL 的 `#separator#` 后；目标上传页用 `stringToDict`（`:2417`）还原。这是浏览器多页面间传状态的 hack，**Go 单进程内不需要**，但说明了一个可复用点：**`raw_info` 应可序列化/反序列化**（Go 版直接 JSON 即可），便于调试、dry-run、断点续传。

### 6.3 对 AutoSeedRelay 的启示

- **保留"每站 enable 开关 + 域名自动纠正"**：YAML 里加 `enable`，加载时按用户自定义覆盖内置注册表；域名变体纠正对多域名站（MTeam、CHDBits、AGSV）很实用。
- **配置文件里的凭据分离**：auto_feed_js 把图床 API key 存本地，Go 版沿用现有 `<PUT_ENV_XXX>` + 环境变量方案即可，无需照搬。
- 脚本的"人审后提交"不适合全自动，但可以借鉴其 **dry-run 预览** 思路：`dictToString` 的输出就是一份可读的"将填写的全部字段"，Go 版的 `preview` 子命令已实现同类能力。

---

## 7. 去重机制

**auto_feed_js 没有客户端去重库**。它的去重由三部分兜底：

1. **目标站上传表单自身判重**：重复种子提交后页面提示"种子已存在"，由浏览器呈现给用户。脚本不拦截。
2. **人工审阅**：脚本只填表，用户提交前肉眼可见分类/标题，重复/不对的直接放弃。
3. **跳转前跨站搜索**：`set_jump_href` mode 2 会给每个目标站生成 `search` 链接（按 imdb/标题），用户可先看目标站是否已有该资源，再决定转不转。

**与 AutoSeedRelay 的差异**：Go 版是守护进程、无人值守，**必须有自动化去重**。当前已有 SQLite（`db_path`）+ infohash 判重的设计是正确的，建议补两点（受 auto_feed_js 启发）：
- **上传前主动查重**：对支持搜索/详情 API 的目标站，先按 `infohash`（若目标站暴露）或规范化标题查一次，命中即跳过，避免触发目标站的"重复种子"错误码。
- **记住"源站→目标站"已转映射**：SQLite 里不仅记 infohash，还记 `(source_site, source_id) → target_site`，防止同源站种子重复转同一目标站（auto_feed_js 靠人眼，Go 版靠表）。

---

## 8. AutoSeedRelay 可直接吸收清单（汇总）

### 种子层（bencode / torrent）
1. 保留清洗集合：**改 announce + 强制 private=1 + 加 source + creation date 抖动 + 补 encoding**（`build_blob_from_torrent`）。
2. **用真 bencode 库重写**，规避字符串正则的缺陷（无 private 键、长度前缀手算）。
3. `source` 字段**放进 info 内**（或做成可配置），避免跨站 infohash 相同被目标站判重。
4. 保留/扩展**禁止转发源站黑名单**（PTP/BLU/BHD/ACM/HDOli/SC/Monika）与"必须先下载一次"前置校验。
5. qB 集成可参考：`/torrents/add` 带 `savepath`/`category`/`skip_checking`，且支持**按源站设置上传限速**。

### 简介层（descr）
6. HTML→BBCode 规则集：免责声明关键词块剥离、懒加载 `data-src` 优先、`[quote=标题]` spoiler 展开、TTG/BW 系 TABLE→quote 等站点覆盖。
7. 归一化清洗：URL 解码、折叠空行、删除空 quote、硬编码防盗链图替换。
8. **small_descr 推导**：`译名/片名` + 季集数 + `类别`。
9. **type 纠偏**：descr 命中 `纪录片/动画` 重分类，映射前执行。
10. **制作组致谢表**：组名正则 → 站点 → 致谢 quote，YAML 化。
11. **MediaInfo 简化**：General/Video/Audio/Text 摘要，per-target 开关。
12. **图片管线**：缩略图→原图 URL 改写表（零成本字符串替换）；catbox 默认图床；`[img]`/`[url][img]` 提取与替换。

### 站点适配层
13. 两级注册表（目标站 ~100 + 源站 ~60）+ **每站 enable 开关 + 域名变体自动纠正**。
14. **国内/国外站点批量默认行为**（`judge_if_the_site_in_domestic`）作为适配器属性。
15. 分类映射：**归一化 type 枚举 + 站点家族共享 type_dict + 单集/多集边界修正 + 兜底**。
16. 维度推断顺序：**标题 → descr → MediaInfo** 三级正则兜底。

### 去重 / 配置
17. 上传前按 infohash/规范化标题查目标站；SQLite 记 `(source_site, source_id) → target_site` 已转映射。
18. `raw_info` 保持可序列化（JSON），供 dry-run 预览与断点续传。

> 一句话总结：auto_feed_js 最值钱的是 **walkDOM 的 HTML→BBCode 规则集、fill_raw_info 的字段推断管线、缩略图→原图改写表、归一化 type 分类映射、以及"源站解析/目标站填表解耦"的中间模型**。Go 版已有 bencode/配置/适配器骨架，缺的正是这些"内容整形"细节。
