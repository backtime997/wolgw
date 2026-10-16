#!/bin/bash
# =============================================================
# wolgw 启动脚本（替换原 /shell/start.sh）
# 特性：
#   1. 启动前自动补执行位（不依赖外层启动命令的 chmod）
#   2. 启动前自检：二进制不可执行 → 打印具体错误并保持容器存活便于排查
#   3. wolgw 输出同时写入 /shell/wolgw.log 和容器日志（iKuai"日志中心"可见）
#   4. 日志超过 5MB 自动截断，防崩溃循环撑爆磁盘
# =============================================================
cd /shell || exit 1
LOG=/shell/wolgw.log

# ---- 清理残留进程（兼容没有 pkill 的镜像：/proc 扫描）----
for p in /proc/[0-9]*/comm; do
  [ "$(cat "$p" 2>/dev/null)" = "wolgw" ] || continue
  pid=${p#/proc/}; pid=${pid%%/*}
  kill "$pid" 2>/dev/null
done
sleep 1

# ---- 日志滚动（>5MB 截断）----
sz=$(stat -c%s "$LOG" 2>/dev/null || echo 0)
[ "$sz" -gt 5242880 ] 2>/dev/null && cp "$LOG" "$LOG.old" 2>/dev/null && : > "$LOG"

echo "===== wolgw start.sh @ $(date) =====" | tee -a "$LOG"
echo "本机架构: $(uname -m)   二进制大小: $(stat -c%s /shell/wolgw 2>/dev/null) 字节" | tee -a "$LOG"

# ---- 确保执行位（防御：外层启动命令的 chmod 可能没生效）----
chmod +x /shell/wolgw 2>/dev/null && echo "执行位: OK" | tee -a "$LOG" || echo "警告: chmod +x 失败" | tee -a "$LOG"
ls -l /shell/wolgw | tee -a "$LOG"

# ---- 自检：二进制能否被本机执行（-h 只触发 flag 解析，不会启动网关）----
out=$(/shell/wolgw -h 2>&1)
rc=$?
if [ "$rc" -ge 126 ]; then
  echo "!!!! 错误: wolgw 无法执行 (rc=$rc): $out" | tee -a "$LOG"
  echo "!!!! 当前机器架构: $(uname -m)" | tee -a "$LOG"
  echo "!!!!  机器是 x86_64  → 请使用 wolgw (amd64 版)" | tee -a "$LOG"
  echo "!!!!  机器是 aarch64 → 请使用 wolgw-arm64" | tee -a "$LOG"
  echo "!!!!  机器是 armv7l  → 请使用 wolgw-arm" | tee -a "$LOG"
  echo "!!!! 也可进容器手动排查: chmod +x /shell/wolgw && /shell/wolgw -h" | tee -a "$LOG"
  echo "!!!! 保持容器存活 1 小时以便排查，修好后请在 iKuai 里重启容器" | tee -a "$LOG"
  sleep 3600
  exit 1
fi
echo "自检通过 (rc=$rc)" | tee -a "$LOG"

# ---- 启动 wolgw：输出 → 日志文件 + 容器日志（iKuai 日志中心）----
echo "启动 wolgw ..." | tee -a "$LOG"
"/shell/wolgw" -config /shell/config.json -listen :80 2>&1 | tee -a "$LOG"
exit ${PIPESTATUS[0]}
