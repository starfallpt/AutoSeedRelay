# AutoSeedRelay v3 Windows 启动脚本
# 用法: .\start.ps1 [full|external]
#   full     - 全套部署（relay + qBittorrent）  [默认]
#   external - 仅 relay，连接已有 qB

param(
    [string]$Mode = "full"
)

$ErrorActionPreference = "Stop"

Write-Host "============================================" -ForegroundColor Cyan
Write-Host "  AutoSeedRelay v3 启动脚本 (Windows)" -ForegroundColor Cyan
Write-Host "============================================" -ForegroundColor Cyan

# 检查 Docker
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host "请先安装 Docker Desktop: https://docs.docker.com/desktop/setup/install/windows-install/" -ForegroundColor Red
    exit 1
}

docker compose version 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) {
    Write-Host "需要 Docker Compose v2，请升级 Docker Desktop" -ForegroundColor Red
    exit 1
}

# 创建必要目录
New-Item -ItemType Directory -Force -Path data\logs, data\downloads | Out-Null

if ($Mode -eq "external") {
    Write-Host ""
    Write-Host "模式: 外部 qB" -ForegroundColor Yellow
    if (-not (Test-Path .env)) {
        @"
QB_HOST=http://192.168.1.100:9021
QB_USER=admin
QB_PASS=yourpassword
"@ | Out-File -FilePath .env -Encoding utf8
        Write-Host "已创建 .env 模板，请修改后重新运行" -ForegroundColor Green
        exit 0
    }
    docker compose -f docker-compose.external.yml up -d
}
else {
    Write-Host ""
    Write-Host "模式: 全套部署" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "  管理面板: http://localhost:9020" -ForegroundColor Green
    Write-Host "  qB WebUI: http://localhost:9020/qb/  (通过 relay 代理)" -ForegroundColor Green
    Write-Host ""

    # Pre-create qBittorrent config with known password
    $pythonCmd = $null
    if (Get-Command python3 -ErrorAction SilentlyContinue) { $pythonCmd = 'python3' }
    elseif (Get-Command python -ErrorAction SilentlyContinue) { $pythonCmd = 'python' }

    if ($pythonCmd) {
        & $pythonCmd scripts\init_qb_config.py CHANGE_ME .\data\qb-config
        if ($LASTEXITCODE -ne 0) {
            Write-Host "WARNING: Failed to pre-create qB config." -ForegroundColor Yellow
        }
    } else {
        Write-Host "WARNING: Python not found, cannot pre-create qB config." -ForegroundColor Yellow
        Write-Host "Check container logs: docker logs qbittorrent" -ForegroundColor Yellow
    }

    docker compose up -d
}

Write-Host ""
Write-Host "============================================" -ForegroundColor Cyan
Write-Host "  启动成功！" -ForegroundColor Green
Write-Host "============================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "首次使用:" -ForegroundColor Yellow
Write-Host "  1. 访问 http://localhost:9020"
Write-Host "  2. 跟随配置向导设置源站/目标站/策略"
Write-Host ""
Write-Host "常用命令:" -ForegroundColor Yellow
Write-Host "  docker compose logs -f relay    # 查看日志"
Write-Host "  docker compose restart relay    # 重启"
Write-Host "  docker compose down             # 停止"
