#!/usr/bin/env bash
# AutoSeedRelay v3 一键启动脚本
# 用法: bash start.sh [external|full]
#   full     - 全套部署（relay + qBittorrent）  [默认]
#   external - 仅 relay，连接已有 qB

set -e

MODE="${1:-full}"
cd "$(dirname "$0")"

echo "============================================"
echo "  AutoSeedRelay v3 启动脚本"
echo "============================================"

# 检查 Docker
if ! command -v docker &>/dev/null; then
    echo "❌ 未安装 Docker，请先安装: https://docs.docker.com/get-docker/"
    exit 1
fi

if ! docker compose version &>/dev/null; then
    echo "❌ 需要 Docker Compose v2，请升级 Docker"
    exit 1
fi

# 创建必要目录
mkdir -p data/logs data/downloads

if [ "$MODE" = "external" ]; then
    echo ""
    echo "📋 模式: 外部 qB（连接已有 qBittorrent）"
    echo ""
    echo "请先编辑 .env 文件设置 qB 连接信息:"
    if [ ! -f .env ]; then
        cat > .env << 'EOF'
# qBittorrent 连接信息
QB_HOST=http://192.168.1.100:9021
QB_USER=admin
QB_PASS=yourpassword
EOF
        echo "  ✅ 已创建 .env 模板，请修改后重新运行"
        exit 0
    fi
    echo "  使用 .env 中的 qB 配置"
    docker compose -f docker-compose.external.yml up -d
else
    echo ""
    echo "📋 模式: 全套部署（relay + qBittorrent）"
    echo ""
    echo "  端口:"
    echo "    管理面板: http://localhost:9020"
    echo "    qB WebUI: http://localhost:9020/qb/  (通过 relay 代理)"
    echo ""

    # Pre-create qBittorrent config with known password
    if command -v python3 &>/dev/null; then
        python3 scripts/init_qb_config.py CHANGE_ME ./data/qb-config
    elif command -v python &>/dev/null; then
        python scripts/init_qb_config.py CHANGE_ME ./data/qb-config
    else
        echo "WARNING: Python not found, cannot pre-create qB config."
        echo "Check container logs: docker logs qbittorrent 2>&1 | grep -i password"
    fi

    docker compose up -d
fi

echo ""
echo "============================================"
echo "  ✅ 启动成功！"
echo "============================================"
echo ""
echo "  首次使用:"
echo "    1. 访问 http://localhost:9020"
echo "    2. 跟随配置向导设置源站/目标站/策略"
echo "    3. 保存后自动开始运行"
echo ""
echo "  常用命令:"
echo "    docker compose logs -f relay    # 查看日志"
echo "    docker compose restart relay    # 重启"
echo "    docker compose down             # 停止"
echo "    bash start.sh external          # 用外部 qB 模式"
echo ""
