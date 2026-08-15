# 标签映射说明（TAG-MAPPING）

> 适用范围：AutoSeedRelay 转种时，如何把源站种子的「标签」翻译成目标站的「标签 / 标签 ID / 标签数组」。
> 源站标签取自详情页（`details.php` 的「标签」行，`internal/detail/detail.go` 的 `ParseTags`），形如 `["官方","国语","中字"]`。

---

## 1. 标签映射的核心原则

### 1.1 同一个标签，在不同目标站含义不同

源站标签（如「官方」）是相对源站定义的：

- 在源站 `internal-source` 上，「官方」表示上传者是本站官方发布组；
- 转发到同家族 dev 站：官方组一致，保留并映射到「官」；
- 转发到 `luckpt` / `M-Team`：这些站点各有自己的官方组，「官方」要么无意义、要么有误导性，应丢弃。

**结论：标签映射必须逐目标站配置，不能一套标签名全站通用。**

### 1.2 归属型标签只在所属站点（或同站点家族）有效

「官方」这类归属型标签，只有在站点自己（或同站点家族的 dev 实例）上才有意义：

- 同站 / 同家族目标（如 dev 站）：保留，映射到对应「官」标签；
- 其它站点（如 luckpt / M-Team）：**转发时应当 `skip`**，因为对方有各自的官方组，带上「官方」有误导性。

其它需要跨站丢弃的标签同理：`独占 / 独家 / 首发 / 自压` 等归属型标签，以及目标站没有对应概念 / 选项的标签。

只保留「客观属性」类标签：语言（国语 / 粤语）、字幕（中字 / 简中 / 繁中）、画质修复（4K修复）、制作方式（原盘 / 内封）等。

### 1.3 标签实现方式因站点架构而异

| 站点架构 | 上传字段 | 取值要求 |
|---------|---------|---------|
| NexusPHP API（`nexusphp`） | `tags[]` 数组 | 值为标签 ID（数字或字符串均可） |
| NexusPHP classic（`nexusphp_classic`） | `tags[4][]` checkbox | **value 必须是数字 ID**，传中文名会失败 |
| M-Team（`mteam`） | `labels` + `tags` 数组 | 字符串标签名，放入对应数组 |

> 实战坑：classic 站点的 `tags[4][]` 是 checkbox 表单，服务端按数字 `tag_id` 校验。
> 直接用「国语」这种中文名当 value 提交，会被服务端当成非法字段值而失败——必须先查标签 ID。
>
> 兜底：若目标 classic 站点不提供标签勾选（或未配置标签映射），relay 会把标签并入简介，
> 如 `[标签:国语,中字]`（见 `internal/targets/base.go`）。需要独立标签位时才走 `tags[4][]`。

---

## 2. 映射表格式（JSON）

顶层 key 是源站标签；每个源站标签下，以**目标站标识**为 key，给出该目标站的动作。

```json
{
  "官方": {
    "dev":    {"tag": "官", "id": 3},
    "luckpt": {"skip": true, "reason": "非本家官方"},
    "mteam":  {"skip": true, "reason": "M-Team 无官方概念"}
  },
  "国语": {
    "dev":    {"tag": "国", "id": 5},
    "luckpt": {"tag": "国语", "id": 5},
    "mteam":  {"field": "labels", "tag": "国语"}
  }
}
```

### 动作字段说明

| 字段 | 说明 | 适用目标 |
|------|------|---------|
| `tag` | 目标站上的标签名 | classic 的 label / API / mteam |
| `id` | 目标站标签的数字 ID | classic 必须（checkbox value）；API 优先 |
| `field` | 目标站数组字段名 | mteam：`labels` 或 `tags` |
| `skip` | `true` = 丢弃该标签 | 任意 |
| `reason` | skip 的原因（写日志 / 告警用） | 任意 |

组合规则：

- `{"tag": "国", "id": 5}`：按 ID 提交（classic / API 都适用）；
- `{"tag": "国语"}`：只按名字提交（API 接受字符串名）；
- `{"field": "labels", "tag": "国语"}`：把「国语」放进 mteam 的 `labels` 数组；省略 `tag` 时直接用源站标签原文；
- `{"skip": true, "reason": "..."}`：不提交该标签。

---

## 3. 实战映射表：internal-source → 各目标站

> 源站：`pt.internal-source.org`。目标站：`dev`（nexusphp_classic）、`luckpt`（nexusphp API）、`mteam`。
> 表内 ID 是各目标站自己的 `tag_id`，互相独立，不可跨站复用（见 §4）。

| 源站标签 | dev站（classic，`tags[4][]`） | luckpt（API，`tags[]`） | mteam（labels / tags） |
|---------|------------------------------|------------------------|-----------------------|
| 官方 | id=3 官 | skip — 非本家官方 | skip |
| 国语 | id=5 国 | id=5 国语 | labels：国语 |
| 粤语 | id=6 粤 | id=6 粤语 | labels：粤语 |
| 中字 | id=1 中字 | id=1 中字 | labels：中字 |
| 简中 | 并入 id=1 中字 | id=2 简中 | labels：简中 |
| 繁中 | 并入 id=1 中字 | id=3 繁中 | labels：繁中 |
| 双语 | 并入 id=1 中字 | id=4 双语 | labels：双语 |
| 中英字幕 | 并入 id=1 中字 | id=7 中英字幕 | labels：中英字幕 |
| 内封字幕 | 并入 id=1 中字 | id=8 内封字幕 | labels：内封字幕 |
| 特效字幕 | skip — dev 无此标签 | id=9 特效字幕 | labels：特效字幕 |
| 国配 | id=4 国配 | id=10 国配 | labels：国配音轨 |
| 4K修复 | id=7 4K修复 | id=11 4K修复 | skip — 画质已由 standard 表达 |
| 外挂字幕 | id=2 外挂 | id=12 外挂字幕 | labels：外挂字幕 |

### 解读

- **官方 → 仅 dev 保留（id=3 官），luckpt / mteam 丢弃**：官方是归属型标签，同站点家族的 dev 保留，转发到其它站一律去掉。
- **简中 / 繁中 / 双语 / 内封 → dev 并入「中字」**：dev 站只有通用「中字」一个勾选项，细分子类统一合并过去，避免丢字幕标签。
- **国语 / 粤语 / 中字 → mteam labels**：语言与字幕属于客观属性，放进 M-Team 的 `labels` 数组。
- **4K修复 → mteam skip**：M-Team 的画质维度由 `standard` 字段表达，重复打标签反而冗余。

> 注意：M-Team 的 `labels` 是预设标签（有可选值）。`labels：国语` 表示「把源站标签『国语』作为 labels 数组的元素」。
> 若目标站预设里没有同名标签，需要先把源站标签名映射成目标站预设名（把 `tag` 字段写成目标站预设值）。

---

## 4. 如何获取目标站标签

**标签 ID 每个站点独立，必须从目标站拉取，不能把 A 站的 ID 复用到 B 站。**

### 4.1 API 站点（NexusPHP API）

拉取上传表单 schema：

```
GET {base_url}/api/v1/sections
Authorization: Bearer {sanctum_token}
```

响应里的 `tags` 数组即标签枚举：

```json
{
  "categories": [ ... ],
  "tags": [
    {"id": 1,  "name": "中字"},
    {"id": 3,  "name": "官"},
    {"id": 5,  "name": "国语"}
  ]
}
```

上传时按 `tags[]` 传 `id`（或 `name`，该 API 两者都接受）。

### 4.2 传统站点（nexusphp_classic）

抓取上传表单页（表单 POST 到 `takeupload.php`）：

```
GET {base_url}/upload.php
Cookie: {登录 cookie}
```

解析标签 checkbox：

```html
<input type="checkbox" name="tags[4][]" value="1" id="tag_1"><label for="tag_1">中字</label>
<input type="checkbox" name="tags[4][]" value="3" id="tag_3"><label for="tag_3">官</label>
<input type="checkbox" name="tags[4][]" value="5" id="tag_5"><label for="tag_5">国</label>
```

- **取 `value` 属性**（数字 ID），不要用 label 文本；
- `name="tags[4][]"` 里的 `4` 是标签组下标，各站不同，以实际表单为准；
- 提交时 `tags[4][] = <value>`（可多个同名参数）。

> 反例：直接把「国语」作为 `tags[4][]` 的 value 提交会失败，服务端按 `(int)` 校验。

### 4.3 M-Team

M-Team 上传接口 `POST /torrent/createOredit` 接受 `labels` 与 `tags` 两个数组：

- `labels`：语言 / 字幕 / 介质等预设标签，值须是 M-Team 预设的标签名；
- `tags`：自由文本标签。

从上传表单 schema（`categoryList` 等端点或网页表单）拿到预设标签列表后，把源站标签名对齐到预设值。

### 4.4 更新流程

1. 每接入一个新目标站，先抓取它的标签枚举（§4.1 / 4.2 / 4.3）；
2. 把每个源站标签对应到目标站 `tag_id` / 预设标签名，或标记 `skip`；
3. 用 `relay preview --target ... --extra tags=国语,中字` 做只读校验，确认 `tags` 字段值正确后再实发。

---

## 5. 配置文件约定：tag_mapping.json

### 5.1 位置与加载

- 默认路径：`config/tag_mapping.json`；
- 转种前加载，把源站 `meta["tags"]`（中文标签名数组）翻译成目标站的上传字段值；
- **未命中映射的源站标签：默认丢弃**，避免把不认识的中文标签塞给目标站。

### 5.2 单源站格式

顶层 key 为源站标签（同 §2）：

```json
{
  "官方": {
    "dev":    {"tag": "官", "id": 3},
    "luckpt": {"skip": true, "reason": "非本家官方"},
    "mteam":  {"skip": true, "reason": "M-Team 无官方概念"}
  },
  "国语": {
    "dev":    {"tag": "国", "id": 5},
    "luckpt": {"tag": "国语", "id": 5},
    "mteam":  {"field": "labels", "tag": "国语"}
  }
}
```

### 5.3 多源站格式

有多个源站时，顶层再加一层源站 key：

```json
{
  "internal-source": {
    "国语": {
      "dev":    {"tag": "国", "id": 5},
      "luckpt": {"tag": "国语", "id": 5},
      "mteam":  {"field": "labels", "tag": "国语"}
    }
  },
  "another-source": {
    "国语": {
      "dev":    {"tag": "国", "id": 5},
      "luckpt": {"tag": "国语", "id": 5},
      "mteam":  {"field": "labels", "tag": "国语"}
    }
  }
}
```

目标站 key（`dev` / `luckpt` / `mteam`）即 `config/relay.yaml` 里 `targets` 列表的站点 `name`。

### 5.4 如何扩展新目标站

以新增目标站 `new-site`（NexusPHP API）为例：

1. **确定架构**：API → 字段 `tags[]`，值用 ID；
2. **抓标签**：`GET {base_url}/api/v1/sections`，记下 `tags` 里每个 `{id, name}`；
3. **补映射**：在每个源站标签对象里加一个 `"new-site"` key：

   ```json
   "国语": {
     "dev":      {"tag": "国", "id": 5},
     "luckpt":   {"tag": "国语", "id": 5},
     "mteam":    {"field": "labels", "tag": "国语"},
     "new-site": {"tag": "国语", "id": 5}
   }
   ```

4. **校验**：`relay preview --target nexusphp --extra tags=国语,中字`，看 `tags` 是否变成目标站 ID；
5. **固化提交**：确认后把映射写进 `config/tag_mapping.json` 并提交到 git。
