# =============================================================
# wolgw — bemfa WOL 唤醒 / SSH 关机网关（纯 Go 单二进制，无运行时依赖）
#
# 构建：docker build -t wolgw .
#   · x86_64 目标机 → dist/wolgw        （默认）
#   · aarch64/arm64 → 先把 dist/wolgw-arm64 覆盖到 dist/wolgw 再构建
#   · armv7l/armv6l → 先把 dist/wolgw-arm  覆盖到 dist/wolgw 再构建
#
# 运行：docker run -d --name wolgw --network=host \
#         -v $PWD/shell:/shell wolgw      # shell/ 持久化配置与日志（可选）
#       Web 界面: http://<宿主机IP>:80
# =============================================================
FROM debian:bookworm-slim
LABEL maintainer="backtime"

WORKDIR /shell
VOLUME shell

COPY start.sh ./start.sh
COPY dist/wolgw ./wolgw
RUN chmod +x /shell/wolgw /shell/start.sh

EXPOSE 80
ENTRYPOINT ["/bin/bash", "./start.sh"]
