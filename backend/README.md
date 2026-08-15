# AutoSeedRelay backend (M0 skeleton)

重构后端骨架：能起服务 + `/api/v2/health` 健康检查 + 登录占位，无业务逻辑。

## 构建（沙箱环境需本地缓存）

```powershell
cd backend
$env:GOCACHE = "$PWD\.gocache"
$env:GOPATH = "$PWD\.gopath"
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:CGO_ENABLED = '0'
$env:GOTELEMETRY = 'off'
go build ./...
go vet ./...
```

`.gocache`/`.gopath` 为本地缓存，已在 `.gitignore` 忽略。

## 启动

```powershell
cd backend
$env:GOPROXY = 'https://goproxy.cn,direct'
go run ./cmd/relay serve
# 可选：go run ./cmd/relay serve -config config.yaml
```

默认监听 `:9020`，健康检查 `GET http://127.0.0.1:9020/api/v2/health`。
登录占位：设置环境变量 `AUTOSEED_WEB_PASSWORD` 后 `POST /api/v2/auth/login` 才可用。
