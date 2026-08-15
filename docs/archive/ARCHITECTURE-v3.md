# AutoSeedRelay v3 架构方案（终版）

---

## 一、部署模式

### 模式 A：全套部署（推荐新手）

```yaml
# docker-compose.yml
services:
  relay:
    build: .
    ports:
      - "9020:9020"         # Web 管理面板
    environment:
      - QB_AUTO_INIT=true   # 自动初始化 qB（改密码/设路径）
    volumes:
      - ./data:/data        # SQLite + 日志 + 下载
    depends_on:
      - qbittorrent

  qbittorrent:
    image: qbittorrentofficial/qbittorrent-nox:latest
    expose:
      - "8080"                   # 仅容器间通信，通过 relay 代理访问
    environment:
      - QBT_WEBUI_PORT=8080
      - QBT_WEBUI_PASSWORD=CHANGE_ME
    volumes:
      - ./data/downloads:/downloads
      - ./data/qb-config:/config
```

**启动流程**：
1. `scripts/init_qb_config.py` 预生成 `qBittorrent.conf`，写入 PBKDF2 密码哈希
2. 容器启动 → qB 读取已有配置文件，使用预设密码（无临时密码）
3. relay 连接 qB（`admin` / `CHANGE_ME`）
4. relay 打开 Web 面板 `:9020` → 用户走配置向导
5. 配置保存 → 开始运行

### 模式 B：外部 qB（已有 qB 的用户）

```yaml
services:
  relay:
    build: .
    ports:
      - "9020:9020"
    environment:
      - QB_HOST=http://192.168.1.100:9021
      - QB_USER=admin
      - QB_PASS=yourpassword
    volumes:
      - ./data:/data
```

---

## 二、qB 会话穿透

Web 面板里点"打开 qB 控制台"，不走 qB 登录页，直接进入：

```
GET /api/qb/proxy → relay 用已存的 qB 凭据获取 SID cookie
                  → 返回 Set-Cookie: SID=xxx
                  → 前端 iframe 加载 qB WebUI，已登录
```

或者直接反向代理：relay 把 `/qb/*` 透明转发到 qB，自动注入 SID。

---

## 三、下载策略

### 3.1 超时重试

```
下载超时（默认 3600s）
  → 暂停种子
  → 等待 retry_interval（默认 300s）
  → 恢复下载
  → 重复 N 次（默认 3 次）
  → 仍然超时 → 放弃，标记 abandoned
```

### 3.2 低速策略

```
检测: qB API dl_speed < 100 KB/s 持续 600s
  → 策略 A（abort）: 放弃下载，标记 slow_abort，释放槽位
  → 策略 B（continue）: 不管，继续挂着等
```

### 3.3 磁盘空间

```
磁盘 < disk_low_gb（默认 50GB）:
  → 暂停所有新任务添加
  → 按"做种时间最长优先"顺序撤旧种子
  → 直到空间恢复到 > disk_low_gb + 20GB

磁盘 < disk_critical_gb（默认 20GB）:
  → 强制撤除所有已完成做种的种子
  → 只保留正在下载中的任务
  → Web 面板红色告警
```

### 3.4 堆积任务

```
同时下载数 > max_concurrent（默认 3）:
  → 新种子排队（paused 状态）
  → 上一个完成后自动启动队列中下一个
  → 队列超过 50 个时告警
```

---

## 四、仪表盘设计

```
┌──────────────────────────────────────────────────────────┐
│  AutoSeedRelay                         [● 运行中] 9020   │
├──────────┬──────────┬──────────┬──────────┬──────────────┤
│ 累计发布  │ 累计辅种  │ 当前做种  │ 磁盘剩余  │ 今日发布/辅种 │
│  1,247   │  4,892   │   89     │ 320 GB  │  +12 / +47  │
├──────────┴──────────┴──────────┴──────────┴──────────────┤
│                                                          │
│  异常队列                                                │
│  ┌──────────────────────────────────────────────────┐   │
│  │ ⚠ 磁盘 320GB → 健康                              │   │
│  │ ⚠ 3 个种子下载超时，等待重试                       │   │
│  │ ⚠ luckpt 站点返回 403，已暂停 5 分钟               │   │
│  └──────────────────────────────────────────────────┘   │
│                                                          │
│  最近活动                                                │
│  ┌──────┬─────────────────────────┬──────┬──────┬─────┐ │
│  │ 时间  │ 标题                    │ 目标  │ 动作  │ 结果 │ │
│  ├──────┼─────────────────────────┼──────┼──────┼─────┤ │
│  │ 09:32│ A Will Eternal S01E171  │ dev  │ 发布  │ ✅  │ │
│  │ 09:28│ Have a Feast S01 2160p  │ dev  │ 辅种  │ ✅  │ │
│  │ 09:15│ Zhang Ga 1963 1440p     │luckpt│ 下载中│ ⏳  │ │
│  └──────┴─────────────────────────┴──────┴──────┴─────┘ │
│                                                          │
│  ┌─ 导航 ───────────────────────────────────────────┐   │
│  │ 仪表盘 │ 种子 │ 源站/目标站 │ qB │ 策略 │ 日志     │   │
│  └──────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────┘
```

---

## 五、配置向导（6 步）

```
Step 1 ─ 源站
  RSS URL + Cookie/Passkey + [测试连接] → RSS 条目预览

Step 2 ─ 目标站
  [+ 添加目标站] → 类型/地址/凭据 → [探测站点] → 自动填分类/标签映射
  → 手动微调 → [Dry Run 预览]

Step 3 ─ qBittorrent
  模式选择: ○ 全套部署(自动初始化)  ○ 外部连接(手动填地址)
  [测试连接] → 显示版本/磁盘空间

Step 4 ─ 筛选策略
  ☑ FREE  ☑ 2X Free  ☐ 50%  ☐ 30%  ☐ 普通
  标题关键词: [StarfallWeb, LongWeb, Pure@StarfallWeb]
  种子大小: 最小 [0] MB  ~ 最大 [0] GB (0=不限)
  角色: ○ 发布者 ○ 辅种者

Step 5 ─ 异常处理
  下载超时: [3600] 秒 | 重试 [3] 次 | 间隔 [300] 秒
  低速阈值: [100] KB/s 持续 [600] 秒 → ○ 放弃 ○ 继续
  磁盘低水位: [50] GB 暂停新任务
  磁盘紧急: [20] GB 强制撤旧种

Step 6 ─ 撤种策略
  ☑ 做种人数 ≥ [5]  撤种
  ☑ 分享率 ≥ [2.0]  撤种
  ☑ 天数 ≥ [14]     撤种
  ☐ 撤种时删除文件

[保存并启动]
```

---

## 六、异常处理矩阵

| 异常 | 检测 | 策略 |
|------|------|------|
| 下载超时 | qB progress 停滞超 N 秒 | 暂停 → 重试 N 次 → 放弃 |
| 低速 | dl_speed < N KB/s 持续 M 秒 | abort 或 continue（可配） |
| 磁盘低 | free < N GB | 暂停新任务，按"做种最久"顺序撤旧种 |
| 磁盘紧急 | free < N GB | 强制撤所有已完成种子 |
| 目标站 403 | HTTP 403 | 退避: 60s → 300s → 900s → 暂停站点 |
| 目标站宕机 | 连续 N 次失败 | 暂停该站 30 分钟 |
| Cookie 过期 | 401/302 | 暂停该站，Web 告警 |
| 种子已存在 | 上传返回"已存在" | 自动切换辅种 |
| 发布冲突 | DB 唯一约束 | 自动降级为辅种 |
| 种子损坏 | bencode 解码失败 | 标记 corrupt，重下 |

---

## 七、目录结构

```
cmd/relay/main.go           # CLI + Web 入口
internal/
├── config/
│   ├── config.go           # 配置结构体
│   └── wizard.go           # 向导逻辑
├── web/
│   ├── server.go           # HTTP 服务
│   ├── api.go              # REST API
│   ├── qb_proxy.go         # qB 会话穿透
│   └── templates/          # HTML 模板
├── engine/
│   ├── engine.go           # 核心引擎
│   ├── cycle.go            # 扫描→决策→执行
│   └── monitor.go          # 监控循环
├── source/
│   ├── rss.go
│   ├── detail.go
│   └── download.go
├── target/
│   ├── target.go           # 接口 + 注册表
│   ├── nexusphp.go
│   ├── classic.go
│   └── mteam.go
├── seed/
│   ├── seed.go
│   ├── bencode.go
│   ├── parser.go
│   └── cleaner.go
├── store/
│   ├── store.go
│   ├── seeds.go
│   ├── records.go
│   └── migrate.go
├── strategy/
│   ├── filter.go           # 促销/大小/关键词
│   └── retire.go           # 撤种判断
├── qb/
│   ├── client.go           # qB API
│   ├── monitor.go          # 速度/磁盘
│   └── autoinit.go         # 自动初始化
├── crossseed/
│   └── crossseed.go
├── imgrehost/
│   └── imgrehost.go
└── descr/
    └── descr.go
```

---

## 八、端口规划

| 服务 | 端口 | 说明 |
|------|------|------|
| relay Web | **9020** | 管理面板 |
| qB WebUI | **9021** | 内部/外部均可 |
| qB BT | **56921** | BT 协议端口（高位） |

全部用 9xxxx 高位段，避开 8080/8081/3000/5000 等常用端口。

---

## 九、实施计划

| 阶段 | 内容 | 预估 |
|------|------|------|
| **P0** | 目录结构 + store 表 + 状态机 | 基础骨架 |
| **P0** | qB 增强（磁盘/速度/状态监控） | 监控能力 |
| **P0** | 异常处理（超时重试/低速/磁盘/403） | 稳定性 |
| **P1** | 策略筛选（促销/大小/关键词） | 核心策略 |
| **P1** | 辅种 crossseed + 撤种 retire | 核心闭环 |
| **P1** | Web 面板（仪表盘 + 配置向导） | 用户体验 |
| **P1** | qB 自动初始化 + 会话穿透 | 部署体验 |
| **P2** | 多人冲突（抢发/降级） | 协作 |
| **P2** | 日志 + 历史查询 + 统计 | 可观测性 |
