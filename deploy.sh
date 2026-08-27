#!/bin/bash
# M365 Copilot2API — Docker 一键部署脚本
# 用法: bash deploy.sh
set -euo pipefail

set +u
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
set -u

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN} M365 Copilot2API Docker 部署脚本${NC}"
echo -e "${GREEN}========================================${NC}"
echo

# 1. 检查 Docker 和 Docker Compose
echo -e "${YELLOW}[1/5] 检查环境...${NC}"
if ! command -v docker &>/dev/null; then
    echo -e "${RED}错误: 未安装 Docker${NC}"
    echo "请先安装 Docker: https://docs.docker.com/engine/install/"
    exit 1
fi

if ! docker compose version &>/dev/null; then
    if command -v docker-compose &>/dev/null; then
        COMPOSE_CMD="docker-compose"
    else
        echo -e "${RED}错误: 未安装 Docker Compose${NC}"
        echo "请先安装 Docker Compose: https://docs.docker.com/compose/install/"
        exit 1
    fi
else
    COMPOSE_CMD="docker compose"
fi

echo -e "${GREEN}  Docker: $(docker --version)${NC}"
echo -e "${GREEN}  Compose: $($COMPOSE_CMD version)${NC}"
echo

# 2. 准备配置文件
echo -e "${YELLOW}[2/5] 准备配置文件...${NC}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 创建 .env 文件
if [ ! -f .env ]; then
    cp .env.example .env
    echo -e "${GREEN}  已创建 .env 文件${NC}"

    # 生成随机 M365_MASTER_KEY
    MASTER_KEY=$(openssl rand -base64 32 2>/dev/null || cat /dev/urandom | head -c 32 | base64)
    if [ -n "$MASTER_KEY" ]; then
        sed -i.bak "s/^M365_MASTER_KEY=$/M365_MASTER_KEY=$MASTER_KEY/" .env && rm -f .env.bak
        echo -e "${GREEN}  已自动生成 M365_MASTER_KEY${NC}"
    fi

    # 提示用户修改密码
    echo
    echo -e "${YELLOW}  ⚠️  请编辑 .env 修改 M365_ADMIN_PASSWORD！${NC}"
    echo -e "${YELLOW}     vim .env${NC}"
    echo
else
    echo -e "${GREEN}  .env 已存在，跳过${NC}"
fi

# 创建 secrets 目录
mkdir -p secrets data

# 如果有管理员密码，写入 secrets 文件
ADMIN_PW=$(grep -E '^M365_ADMIN_PASSWORD=' .env | cut -d'=' -f2- || true)
if [ -n "$ADMIN_PW" ]; then
    echo -n "$ADMIN_PW" > secrets/m365_admin_password
    chmod 600 secrets/m365_admin_password
    echo -e "${GREEN}  已写入管理员密码到 secrets/m365_admin_password${NC}"
fi

echo

# 3. 构建并启动
echo -e "${YELLOW}[3/5] 构建 Docker 镜像...${NC}"
$COMPOSE_CMD build
echo

echo -e "${YELLOW}[4/5] 启动容器...${NC}"
$COMPOSE_CMD up -d
echo

# 5. 等待健康检查
echo -e "${YELLOW}[5/5] 等待服务启动...${NC}"
sleep 5

# 获取服务器 IP
SERVER_IP=$(curl -s ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")

for i in $(seq 1 10); do
    if curl -sf "http://localhost:80/api/health" >/dev/null 2>&1; then
        echo
        echo -e "${GREEN}========================================${NC}"
        echo -e "${GREEN} 部署成功！${NC}"
        echo -e "${GREEN}========================================${NC}"
        echo
        echo -e "  服务地址:  ${GREEN}http://$SERVER_IP${NC}"
        echo -e "  控制台:    ${GREEN}http://$SERVER_IP${NC}"
        echo
        echo -e "  ${YELLOW}Cloudflare DNS 设置:${NC}"
        echo -e "    A 记录 → $SERVER_IP"
        echo -e "    端口 → 80 (CF 代理可开启自动 HTTPS)"
        echo
        echo -e "  ${YELLOW}管理命令:${NC}"
        echo -e "    查看日志:   $COMPOSE_CMD logs -f"
        echo -e "    重启服务:   $COMPOSE_CMD restart"
        echo -e "    停止服务:   $COMPOSE_CMD down"
        echo -e "    更新代码:   git pull && $COMPOSE_CMD up -d --build"
        echo
        exit 0
    fi
    sleep 2
done

echo
echo -e "${RED}服务启动超时，请检查日志:${NC}"
$COMPOSE_CMD logs --tail 30
exit 1
