# AutoSeedRelay v4 — 多阶段构建：前端 Vue → 后端 Go(embed) → alpine 单容器
# 构建：docker build -t autoseedrelay .
# 说明：CI 由 .github/workflows/ci.yml 驱动（GitHub Actions + GHCR 发布）

# ---------- stage 1: 前端构建 ----------
FROM node:20-alpine AS fe
WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ---------- stage 2: 后端构建（内嵌前端产物） ----------
FROM golang:1.22-alpine AS be
WORKDIR /build/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=fe /build/frontend/dist ./internal/webfs/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o relay ./cmd/relay

# ---------- stage 3: 运行 ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=be /build/backend/relay /usr/local/bin/relay
VOLUME /data
EXPOSE 9020
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:9020/api/v2/health || exit 1
ENTRYPOINT ["relay"]
CMD ["serve"]
