# AutoSeedRelay 当前状态报告

## 一、部署链路

```
开发机 push → GitHub → GitHub Actions 打包 Docker 镜像 → 推送到 ghcr.io
                                                              ↓
VPS: docker compose pull relay → docker compose up -d relay
```

**VPS 信息**: 1.2.3.4:XXXX, /opt/AutoSeedRelay

## 二、密码问题

### 当前密码: `REDACTED` (Web 和 qB 同一个)

### 为什么老是出问题:

1. **qBittorrent 不支持通过环境变量或配置文件预设密码**
   - linuxserver/qbittorrent: 无密码环境变量
   - qbittorrentofficial/qbittorrent-nox: 无密码环境变量
   - PBKDF2 预生成: 不兼容 qB v5.x 的哈希格式
   - **唯一办法**: qB 启动后产生临时密码 → 调 API 设置为永久密码

2. **当前部署脚本的流程**:
   ```
   docker compose up → qB 生成临时密码 → relay 用临时密码连 qB
   ```
   问题: qB 容器重建时临时密码会变，relay 配置里的是旧密码

3. **Setup 向导保存时把密码写成了 `'***'`**
   - 密码输入框的掩码符被当成真实密码保存了

### 怎么彻底解决:

方案 A: 把 `docker compose up` 替换为部署脚本:
   ```bash
   # 1. 启动 qB
   docker compose up -d qbittorrent
   # 2. 等 qB 就绪，从日志提取临时密码
   TMP=$(docker logs qbittorrent | grep temporary | awk '{print $NF}')
   # 3. 调 qB API 设为固定密码 CHANGE_ME
   curl ... /api/v2/app/setPreferences -d 'json={"web_ui_password":"CHANGE_ME"}'
   # 4. 写 relay 配置用固定密码
   # 5. 启动 relay
   docker compose up -d relay
   ```

方案 B: 修好 PBKDF2 预生成，找到 qB 5.x 正确的哈希格式

## 三、已验证可用的功能

| 功能 | 状态 |
|------|------|
| Web 面板登录 | ✅ |
| 仪表盘 | ✅ |
| 配置页 (标签栏) | ✅ |
| Setup 向导 | ✅ |
| RSS 预览 | ✅ (4519 bytes 数据) |
| 源站连接 | ✅ |
| CI/CD 自动打包 | ✅ |
| GHCR 公开拉取 | ✅ |

## 四、待修复

| 问题 | 优先级 |
|------|--------|
| qB 代理 `/qb/` — SID 提取失败 | P0 |
| qB 密码持久化 | P0 |
| setup 保存密码 `***` bug | P1 |
| 端到端转种测试 | P1 |
| 预览页转发字段展示 | P2 |

## 五、数据存储

```
/opt/AutoSeedRelay/
├── config/relay.yaml           # 配置文件
├── data/
│   ├── relay.db                # SQLite 数据库 (40KB)
│   ├── downloads/              # qB 下载目录
│   └── qb-config/              # qB 配置 (PBKDF2密码等)
└── docker-compose.yml
```

## 六、重启后如何恢复

如果有问题，SSH 到 VPS 跑:
```bash
cd /opt/AutoSeedRelay
TMP=$(docker logs qbittorrent 2>&1 | grep temporary | tail -1 | awk '{print $NF}')
sed -i "s/password:.*/password: $TMP/" config/relay.yaml
docker restart autoseedrelay
```
