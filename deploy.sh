#!/bin/bash
# =============================================================
# 一键部署：把 wolgw 上传到运行中的容器并替换 start.sh
# 用法: ./deploy.sh <容器名或ID>
# 前提: 容器由原 Dockerfile 镜像创建（/shell 为 VOLUME，文件会持久保留）
# =============================================================
set -e
CONTAINER=${1:?用法: ./deploy.sh <容器名或ID>}
DIR=$(cd "$(dirname "$0")" && pwd)

# 1. 确认容器存在
docker inspect "$CONTAINER" >/dev/null 2>&1 || { echo "错误: 找不到容器 $CONTAINER"; exit 1; }

# 2. 检测容器内架构
ARCH=$(docker exec "$CONTAINER" sh -c 'uname -m' 2>/dev/null || echo "unknown")
echo "容器架构: $ARCH"
case "$ARCH" in
  x86_64|amd64)     BIN="$DIR/dist/wolgw" ;;
  aarch64|arm64)    BIN="$DIR/dist/wolgw-arm64" ;;
  armv7l|armv6l|armv5tel) BIN="$DIR/dist/wolgw-arm" ;;
  *) echo "警告: 架构 $ARCH 无法识别，尝试 amd64 二进制"; BIN="$DIR/dist/wolgw" ;;
esac
[ -f "$BIN" ] || { echo "错误: 未找到 $BIN，请先编译（见 README「重新编译」）"; exit 1; }

# 3. 上传文件
docker cp "$BIN" "$CONTAINER":/shell/wolgw
docker cp "$DIR/start.sh" "$CONTAINER":/shell/start.sh
docker exec "$CONTAINER" chmod +x /shell/wolgw /shell/start.sh
echo "已上传: /shell/wolgw, /shell/start.sh"

# 4. 重启容器（entrypoint 会执行新的 start.sh）
docker restart "$CONTAINER"
sleep 3

# 5. 显示结果
echo
echo "=== 容器日志 ==="
docker logs --tail 15 "$CONTAINER" 2>&1 || true
echo
echo "=== 容器内进程 ==="
docker exec "$CONTAINER" ps -ef 2>/dev/null || docker top "$CONTAINER" 2>/dev/null || true
echo
echo "====================================================="
echo " 完成！Web 界面: http://<宿主机IP>"
echo " 日志文件: 容器内 /shell/wolgw.log"
echo "====================================================="
