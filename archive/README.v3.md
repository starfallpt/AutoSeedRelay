# AutoSeedRelay

<p align="center">
  <strong>PT 站点自动辅种平台</strong><br>
  源站 RSS 监控 → 自动下载 → 清洗适配 → 多目标站发布/辅种 → 做种监控 → 自动撤种
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.22-00ADD8?logo=go" alt="Go 1.22">
  <img src="https://img.shields.io/badge/license-MIT-green" alt="MIT License">
  <img src="https://img.shields.io/badge/docker-ready-blue?logo=docker" alt="Docker">
</p>

---

## 功能

- **自动转种**：源站 RSS 扫描 → 命中关键词 → 下载种子 → 提取详情（MediaInfo/标签/简介）→ 清洗适配 → 上传目标站
- **智能辅种**：目标站种子已存在时，自动下载目标站种子交叉做种
- **多人协作**：多人同时运行，原子抢发，其余自动降级为辅种
- **策略筛选**：按促销类型（FREE/2X Free/50%/30%）、种子大小、关键词筛选
- **自动撤种**：做种人数达标 / 分享率达标 / 天数上限 / 磁盘紧急 四种策略自动撤种
- **图片转存**：简介中的外部图片自动转存到自有图床
- **Web 管理面板**：仪表盘 / 种子列表 / 配置向导 / qB 会话穿透 / 实时日志
- **异常处理**：下载超时重试 / 低速放弃 / 磁盘满暂停 / 站点限流退避 / Cookie 过期告警

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/starfallpt/AutoSeedRelay.git
cd AutoSeedRelay

# 全套部署（relay + qBittorrent）
bash start.sh

# 或连接已有 qB
bash start.sh external   # 先编辑 .env 填 qB 地址
```

访问 `http://localhost:9020`，跟随配置向导完成设置。

### 源码运行

```bash
# 需要 Go 1.22+
go run ./cmd/relay serve --config config/relay.yaml
```

## 端口

| 服务 | 端口 | 说明 |
|------|------|------|
| 管理面板 | 9020 | Web 配置 + 监控 |
| qB WebUI | 9021 | BT 客户端（面板内免密直达） |
| qB BT | 56921 | P2P 协议端口 |

## 支持的站点类型

| 类型 | 上传方式 | 鉴权 |
|------|---------|------|
| NexusPHP API (>=1.9) | `/api/v1/upload` | Sanctum Bearer |
| NexusPHP 传统 (<1.9) | `takeupload.php` 表单 | Cookie |
| M-Team | `/torrent/createOredit` | `x-api-key` |

新增站点 = 一个文件 + 一个 YAML 配置。

## 文档

| 文档 | 说明 |
|------|------|
| [架构方案](docs/ARCHITECTURE-v3.md) | 完整架构设计 |
| [站点适配指南](docs/SITE-ADAPTER.md) | 如何接入新目标站 |
| [标签映射说明](docs/TAG-MAPPING.md) | 源站标签 → 目标站标签映射规则 |
| [M-Team API 参考](docs/MTEAM-API.md) | M-Team 接口字段说明 |
| [转种字段对照](docs/RELAY-FIELDS.md) | 各站点必填/可选字段速查 |
| [PTNexus 参考](docs/REF-PTNexus.md) | 参考项目分析 |
| [auto_feed_js 参考](docs/REF-auto-feed-js.md) | 参考项目分析 |

## 配置

首次运行通过 Web 配置向导完成，或手动编辑 `config/relay.yaml`：

```yaml
source:
  rss_url: "https://源站/torrentrss.php?passkey=..."

targets:
  - name: dev
    type: classic
    base_url: "https://目标站"
    tags:
      国语: { field: "tags[4][]", id: 5 }
      中字: { field: "tags[4][]", id: 6 }

qbittorrent:
  host: "http://qbittorrent:9021"

filter:
  keywords: ["StarfallWeb"]
  promotions: ["free", "2xfree"]

retire:
  min_seeders: 5
  max_days: 14
```

敏感凭据通过环境变量注入（`<PUT_ENV_XXX>` 占位符或 `AUTOSEED_<SITE>_<FIELD>` 约定）。

## 开发

```bash
# 编译
CGO_ENABLED=0 go build -o relay ./cmd/relay/

# 运行测试
CGO_ENABLED=0 go test ./...

# 开发模式
go run ./cmd/relay serve --config config/relay.yaml --dev
```

## 许可

MIT License - 详见 [LICENSE](LICENSE)
