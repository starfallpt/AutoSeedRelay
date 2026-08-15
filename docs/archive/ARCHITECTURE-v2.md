# AutoSeedRelay v2 架构方案

> 从"单人转种脚本"升级为"多人协作自动辅种平台"

## 一、整体架构

```
┌─────────────────────────────────────────────────────┐
│                    cmd/relay                        │
│              CLI 入口 + 配置文件                     │
└──────────────┬──────────────┬───────────────────────┘
               │              │
     ┌─────────▼──┐    ┌─────▼──────┐
     │  scheduler  │    │   web UI   │  (可选，后期)
     │  定时调度    │    │  管理面板   │
     └─────┬───────┘    └────────────┘
           │
     ┌─────▼──────────────────────────────────┐
     │              engine                     │
     │  核心引擎：扫描→决策→执行→监控          │
     └──┬────────┬────────┬────────┬──────────┘
        │        │        │        │
   ┌────▼──┐ ┌──▼───┐ ┌──▼───┐ ┌──▼──────────┐
   │source │ │target│ │ seed  │ │  strategy    │
   │ 源站   │ │目标站│ │ 种子   │ │  用户策略     │
   │ 抓取   │ │ 发布  │ │ 管理   │ │  筛选/撤种    │
   └───────┘ └──────┘ └──────┘ └──────────────┘
```

## 二、目录结构

```
cmd/relay/main.go           # CLI 入口
internal/
├── config/
│   └── config.go            # 全局配置 + 用户策略
├── engine/
│   └── engine.go            # 核心引擎：串联所有模块
├── source/
│   ├── rss.go               # RSS 抓取
│   ├── detail.go            # 详情页抓取（MediaInfo/简介/标签）
│   └── download.go          # 种子下载（direct/qB）
├── target/
│   ├── target.go            # 统一接口 + 注册表
│   ├── nexusphp.go          # NexusPHP >=1.9 API
│   ├── classic.go           # 传统 takeupload.php
│   └── mteam.go             # M-Team
├── seed/
│   ├── seed.go              # 种子清洗
│   ├── bencode.go           # bencode 编解码
│   └── parser.go            # 种子解析
├── store/
│   ├── store.go             # SQLite 持久化
│   ├── history.go           # 历史记录 CRUD
│   └── migrate.go           # 数据库迁移
├── strategy/
│   └── strategy.go          # 用户策略：筛选/撤种条件
├── monitor/
│   └── monitor.go           # 目标站状态监控（做种/下载数）
├── crossseed/
│   └── crossseed.go         # 辅种逻辑（下载已有种子→跳过校验→做种）
├── qb/
│   └── qb.go                # qBittorrent 客户端
├── imgrehost/
│   └── imgrehost.go         # 图片转存
└── descr/
    └── descr.go             # 简介构造 + HTML→BBCode
```

## 三、核心数据模型

### 3.1 种子生命周期状态机

```
                     ┌──────────────┐
                     │  discovered  │  RSS 扫描到
                     └──────┬───────┘
                            │
                     ┌──────▼───────┐
                     │ downloading  │  正在下载 .torrent
                     └──────┬───────┘
                            │
              ┌─────────────┼─────────────┐
              │             │             │
     ┌────────▼───┐  ┌──────▼──────┐  ┌──▼──────────┐
     │ publishing │  │ cross_seed  │  │   skipped    │
     │  发布中     │  │  准备辅种    │  │  不符合策略   │
     └────┬───────┘  └──────┬──────┘  └──────────────┘
          │                 │
     ┌────▼──────┐   ┌──────▼──────┐
     │ published │   │  seeding    │  已在目标站做种
     └────┬──────┘   └──────┬──────┘
          │                 │
          └────────┬────────┘
                   │
            ┌──────▼──────┐
            │  monitoring │  监控中（定期检查做种/下载数）
            └──────┬──────┘
                   │
       ┌───────────┼───────────┐
       │           │           │
  ┌────▼───┐ ┌────▼───┐ ┌─────▼────┐
  │seeding │ │retired │ │  failed   │
  │ 出种完成│ │ 已撤种  │ │  异常失败  │
  └────────┘ └────────┘ └───────────┘
```

### 3.2 SQLite 表设计

```sql
-- 核心：种子流转记录
CREATE TABLE seeds (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    info_hash     TEXT NOT NULL,           -- 源站 infohash
    source_site   TEXT NOT NULL,           -- 源站标识
    source_id     INTEGER,                 -- 源站种子 ID
    title         TEXT,                    -- 标题
    size          INTEGER,                 -- 字节
    category      TEXT,                    -- 分类
    promotion     TEXT,                    -- 促销类型: free/2xfree/50%/30%/normal
    publish_time  TEXT,                    -- 源站发布时间
    discovered_at TEXT,                    -- 扫描发现时间
    downloaded_at TEXT,                    -- 下载完成时间
    status        TEXT NOT NULL DEFAULT 'discovered',
    error_msg     TEXT,                    -- 失败原因
    created_at    TEXT DEFAULT (datetime('now')),
    updated_at    TEXT DEFAULT (datetime('now'))
);

-- 每个目标站的发布/辅种记录
CREATE TABLE relay_records (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_id       INTEGER REFERENCES seeds(id),
    target_site   TEXT NOT NULL,           -- 目标站标识
    target_id     INTEGER,                 -- 目标站种子 ID
    target_hash   TEXT,                    -- 清洗后的 infohash
    role          TEXT NOT NULL,           -- publisher / seeder
    published_at  TEXT,                    -- 发布时间
    cross_seeded_at TEXT,                  -- 辅种开始时间
    status        TEXT NOT NULL DEFAULT 'pending',
    seeders       INTEGER DEFAULT 0,       -- 当前做种数
    leechers      INTEGER DEFAULT 0,       -- 当前下载数
    last_check_at TEXT,                    -- 最后检查时间
    retired_at    TEXT,                    -- 撤种时间
    retire_reason TEXT,                    -- 撤种原因: seeded_enough / timeout / manual
    created_at    TEXT DEFAULT (datetime('now')),
    updated_at    TEXT DEFAULT (datetime('now')),
    UNIQUE(seed_id, target_site)
);

-- 操作用户记录
CREATE TABLE users (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    name     TEXT NOT NULL UNIQUE,         -- 用户名
    role     TEXT NOT NULL DEFAULT 'seeder' -- publisher / seeder / admin
    -- publisher: 可以抢发布
    -- seeder: 只辅种不发布
    -- admin: 全部权限
);

-- 执行日志
CREATE TABLE activity_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_id    INTEGER,
    action     TEXT NOT NULL,              -- discovered/downloaded/published/cross_seeded/monitored/retired/failed
    detail     TEXT,                       -- JSON 详情
    created_at TEXT DEFAULT (datetime('now'))
);
```

## 四、用户策略配置

```yaml
# config/strategy.yaml
user:
  name: "我的组员名"
  role: publisher              # publisher | seeder

promotion_filter:
  # 只处理这些促销类型的种子
  free: true                   # FREE 种子
  free_2x: true                # 2X Free
  half_off: false              # 50%
  thirty_pct: false            # 30%
  normal: false                # 普通种子（无促销）

cross_seed:
  enabled: true                # 启用辅种（种子已存在时下载辅种）
  max_concurrent: 5            # 同时辅种上限

retire_policy:
  # 撤种条件（满足任一即撤种）
  min_seeders: 5               # 目标站做种人数 >= 5
  min_ratio: 2.0               # 上传/下载比 >= 2.0
  max_days: 14                 # 最多做种 14 天
  disk_quota_gb: 500           # 磁盘配额上限

# 发布抢发策略（publisher 角色）
publish:
  max_wait_seconds: 300        # 扫描到后最多等 300 秒（给其他人抢发机会）
  retry_on_fail: 3             # 失败重试次数

# qBittorrent
qbittorrent:
  host: "http://localhost:8080"
  username: "admin"
  password: "${QB_PASSWORD}"
  download_path: "/data/downloads"
```

## 五、核心流程

### 5.1 主循环

```go
func (e *Engine) Run(ctx context.Context) error {
    ticker := time.NewTicker(e.config.PollInterval)
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            e.runCycle()
            e.monitorAll()  // 检查所有做种中的状态
        }
    }
}

func (e *Engine) runCycle() {
    // 1. 扫描源站 RSS
    items := e.source.FetchRSS()
    for _, item := range items {
        // 2. 关键词筛选
        if !item.MatchesKeywords(e.config.Keywords) {
            continue
        }
        // 3. 去重检查
        if e.store.Exists(item.InfoHash) {
            continue
        }
        // 4. 促销策略筛选
        if !e.strategy.MatchPromotion(item) {
            e.store.Record(item, "skipped", "promotion_filter")
            continue
        }
        // 5. 新建记录
        seed := e.store.Insert(item, "discovered")

        // 6. 检查目标站是否已有
        existing := e.checkTargetSites(seed)
        if existing != nil {
            // 已存在 → 交叉辅种
            go e.crossSeed(seed, existing)
        } else if e.user.Role == "publisher" {
            // 我是发布者 → 抢发
            go e.publish(seed)
        }
        // seeder 角色且种子不存在 → 等待别人发布后辅种
    }
}
```

### 5.2 发布流程

```go
func (e *Engine) publish(seed *Seed) {
    // 1. 随机等待（多人抢发时分散冲突）
    jitter := rand.Intn(e.config.Publish.MaxWaitSeconds)
    time.Sleep(time.Duration(jitter) * time.Second)

    // 2. 再次检查是否已被发
    if e.store.AlreadyPublished(seed, targetSite) {
        return e.crossSeed(seed, existing)
    }

    // 3. 下载种子
    torrentPath := e.source.Download(seed)
    seed.DownloadedAt = time.Now()

    // 4. 获取详情（MediaInfo/简介/标签）
    detail := e.source.FetchDetail(seed.SourceID)

    // 5. 图片转存
    descr := e.imgrehost.Process(detail.Descr)

    // 6. 清洗种子
    cleaned := seed.Clean(targetSite)

    // 7. 构建表单字段
    fields := target.BuildFields(cleaned, detail)

    // 8. 上传
    result := target.Upload(cleaned.Path, fields)

    // 9. 记录
    e.store.MarkPublished(seed, targetSite, result.TargetID)

    // 10. qB 添加做种
    e.qb.AddTorrent(cleaned.Path, skip_checking=true)
}
```

### 5.3 辅种流程

```go
func (e *Engine) crossSeed(seed *Seed, existing *ExistingSeed) {
    // 1. 下载目标站种子
    targetTorrent := target.DownloadTorrent(existing.TargetID)

    // 2. qB 添加，指向已有数据目录，skip_checking
    e.qb.AddTorrent(targetTorrent.Path,
        savepath=seed.DataPath,
        skip_checking=true,
        category="cross-seed",
    )

    // 3. 记录
    e.store.MarkCrossSeeded(seed, targetSite)
}
```

### 5.4 监控与撤种

```go
func (e *Engine) monitorAll() {
    seeds := e.store.GetActiveSeeds()  // status = published | cross_seeded | seeding
    for _, s := range seeds {
        for _, rec := range s.RelayRecords {
            // 获取目标站种子状态
            stats := target.GetSeedStats(rec.TargetID)

            // 更新做种/下载数
            rec.Seeders = stats.Seeders
            rec.Leechers = stats.Leechers
            rec.LastCheckAt = time.Now()
            e.store.UpdateRecord(rec)

            // 检查撤种条件
            if e.strategy.ShouldRetire(rec) {
                e.qb.DeleteTorrent(rec.TargetHash)
                rec.Status = "retired"
                rec.RetiredAt = time.Now()
                rec.RetireReason = e.strategy.RetireReason(rec)
                e.store.UpdateRecord(rec)
            }
        }
    }
}
```

## 六、多人协作冲突处理

```go
// 多人同时扫描到同一种子时，谁先发布谁赢
func (e *Engine) claimPublish(seed *Seed, targetSite string) (bool, error) {
    // SQLite 原子操作：INSERT OR IGNORE
    result := e.store.db.Exec(`
        INSERT INTO relay_records (seed_id, target_site, role, status)
        VALUES (?, ?, 'publisher', 'publishing')
        ON CONFLICT(seed_id, target_site) DO NOTHING
    `, seed.ID, targetSite)
    if result.RowsAffected == 0 {
        return false, nil  // 别人抢到了，我转辅种
    }
    return true, nil
}
```

## 七、CLI 设计

```bash
# 启动守护进程
relay run --config config/strategy.yaml

# 查看状态
relay status                    # 当前做种/发布统计
relay history --limit 50        # 最近 50 条记录
relay history --seed 172843     # 某个种子的完整流转

# 管理
relay retire --target dev --all # 撤掉所有 dev 站做种
relay retry --id 123            # 重试失败的发布
```

## 八、实施计划

| 阶段 | 内容 | 优先级 |
|------|------|--------|
| P0 | store 表设计 + 状态机 | 基础 |
| P0 | 促销筛选 strategy | 核心策略 |
| P1 | 完整的 publish 流程 | 核心 |
| P1 | 交叉辅种 crossseed | 核心 |
| P1 | 监控 + 撤种 monitor | 核心 |
| P2 | 多人冲突处理 | 协作 |
| P2 | 历史查询 CLI | 体验 |
| P3 | Web UI | 可选 |
