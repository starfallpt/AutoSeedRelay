# 部署指南

## 方式一：Docker Compose 全套部署（推荐）

适用于新服务器，一键部署 relay + qBittorrent。

```bash
git clone https://github.com/starfallpt/AutoSeedRelay.git
cd AutoSeedRelay
bash start.sh
```

启动后：
- 管理面板：`http://<服务器IP>:9020`
- qB WebUI：`http://<服务器IP>:9021`

qB 默认密码 `CHANGE_ME`，首次登录后建议通过 Web 面板修改。
qB 会自动初始化（修改默认密码、设置下载路径），无需手动配置。

## 方式二：连接已有 qBittorrent

已有 qB 实例时使用。

```bash
git clone https://github.com/starfallpt/AutoSeedRelay.git
cd AutoSeedRelay
cp docker-compose.external.yml docker-compose.override.yml
```

编辑 `.env`：
```env
QB_HOST=http://192.168.1.100:9021
QB_USER=admin
QB_PASS=你的密码
```

```bash
docker compose up -d
```

## 方式三：Docker 镜像

从 GitHub Container Registry 拉取预构建镜像：

```bash
docker pull ghcr.io/starfallpt/autoseedrelay:latest

docker run -d \
  --name autoseedrelay \
  -p 9020:9020 \
  -v ./data:/data \
  -e QB_HOST=http://192.168.1.100:9021 \
  -e QB_USER=admin \
  -e QB_PASS=密码 \
  ghcr.io/starfallpt/autoseedrelay:latest serve
```

## 方式四：源码运行

```bash
# 需要 Go 1.22+
git clone https://github.com/starfallpt/AutoSeedRelay.git
cd AutoSeedRelay
CGO_ENABLED=0 go build -o relay ./cmd/relay/
./relay serve --config config/relay.yaml
```

## 目录结构

```
/data
├── relay.db          # SQLite 数据库
├── logs/             # 日志文件（每天轮转）
├── downloads/        # qB 下载目录
└── qb-config/        # qB 配置（全套部署时）
```

## 更新

```bash
# Docker Compose 部署
git pull
docker compose up -d --build

# 镜像部署
docker pull ghcr.io/starfallpt/autoseedrelay:latest
docker stop autoseedrelay && docker rm autoseedrelay
docker run -d ...  # 同上
```

## 卸载

```bash
docker compose down -v   # 删除容器 + 数据卷
rm -rf ./data            # 删除本地数据
```

## 防火墙

需要开放以下端口：

| 端口 | 协议 | 用途 |
|------|------|------|
| 9020 | TCP | Web 管理面板 |
| 9021 | TCP | qB WebUI（可选） |
| 56921 | TCP/UDP | BT 协议 |
