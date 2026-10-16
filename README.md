# wolgw — bemfa WOL 唤醒 / SSH 关机网关（多设备 · 纯 Go 单二进制）

自包含仓库：**Dockerfile + 启动脚本 + 三架构预编译二进制 + 全部源码**，`docker build` 即可出镜像；
也支持向已有容器热替换部署（`deploy.sh`，上传 `dist/wolgw*` + `start.sh` 到 `/shell/`）。

全部功能由单个静态 Go 二进制完成（约 6MB，无任何运行时依赖），按目标机架构选择：

| 文件 | 适用宿主 |
|---|---|
| `dist/wolgw` | x86_64 / amd64 |
| `dist/wolgw-arm64` | aarch64（ARM 服务器、NAS） |
| `dist/wolgw-arm` | armv7l / armv6l（老路由器、树莓派 1/2） |

| 功能 | 说明 |
|---|---|
| bemfa TCP 接入 | 按[巴法 TCP 协议](https://cloud.bemfa.com/docs/src/tcp.html)连接 `bemfa.com:8344`，订阅设备主题、心跳保活、断线自动重连 |
| 多设备管理 | 一个 uid 下管理 N 台目标设备，**每台设备独立 topic 与配置**（IP/MAC/SSH/关机命令）；指令按 topic 路由到对应设备；单条订阅最多 8 个 topic，超出自动分批 |
| 云端纯接收 | 与 bemfa 的交互**只收不发**：订阅设备主题 + `cmd=9` 每 5s 轮询拉取最新指令 + ping 心跳保活（45s）；网关不向云端推送任何信息（不报 online、不发布操作结果），结果仅本地 Web/API 展示；断线自动重连 |
| WOL 唤醒 | 发送标准魔术包（6×FF + 16×MAC），每次自动发往 **设备配置的广播地址 + 目标 IP 的 /24 网段定向广播（如 `192.168.3.x → 192.168.3.255`）+ 目标 IP**（去重），同网段/跨网段自适应；仅唤醒 MAC 匹配的那一台，不会误唤醒其他设备 |
| SSH 关机 | 内置 SSH 客户端（用户名+密码），执行自定义关机命令；`sudo -S` 自动喂密码 |
| Web 界面 | 配置 bemfa + 多台设备，按设备手动触发唤醒/关机，实时状态与日志 |

## 文件清单

```
wolgw/
├── Dockerfile       # 自包含镜像构建（debian:bookworm-slim + start.sh + dist/wolgw）
├── docker-compose.yml # compose 部署（build 本仓库；host 网络 + ./shell:/shell）
├── dist/            # 预编译二进制：wolgw(amd64) / wolgw-arm64(aarch64) / wolgw-arm(armv7)
├── start.sh         # 启动脚本（自检、日志滚动、-listen :80，SIGTERM 秒停）
├── deploy.sh        # 已有容器热替换脚本（自动按容器架构选二进制 + docker cp + 重启）
├── src/             # Go 源码（main.go + static/index.html）
└── README.md
```

## 部署（在 x86 宿主机上执行）

### 方法一：docker compose（推荐）

```bash
# 1. 把 docker-compose.yml 和 dist/wolgw、start.sh 放到宿主机同一目录
# 2. 建 shell 目录并放入文件（按宿主机架构选二进制：x86_64→wolgw / aarch64→wolgw-arm64 / armv7l→wolgw-arm）
mkdir -p shell
cp dist/wolgw shell/ start.sh shell/    # 架构不对就把对应的 dist/wolgw-arm64 重命名为 wolgw
# 3. 启动（compose 会先 chmod +x 补执行权限，再启动 start.sh）
docker compose up -d
# 4. 打开 Web 界面配置设备（config.json 由页面"保存全部配置"生成，
#    也可把旧容器的 /shell/config.json 拷进 shell/）
```

compose 要点：
- `entrypoint` 覆盖镜像原设计：`chmod +x /shell/wolgw /shell/start.sh` → `exec /bin/bash /shell/start.sh`
- `network_mode: host`：WOL 广播包必须走 host 网络才能到达局域网
- `./shell:/shell` 绑定挂载：`wolgw.log` 日志直接写回宿主机此目录
- `restart: unless-stopped` + wolgw 作为容器 PID 1：
  `docker stop` 秒停；wolgw 崩溃 → 容器退出 → 自动拉起

### 方法二：已有容器热替换（不新建容器）

```bash
# 一键部署（自动 docker cp + 重启）
./deploy.sh <容器名>

# 或手动
docker cp dist/wolgw  <容器名>:/shell/wolgw
docker cp start.sh    <容器名>:/shell/start.sh
docker exec <容器名> chmod +x /shell/wolgw /shell/start.sh
docker restart <容器名>
```

> `/shell` 是镜像里声明的 VOLUME，`docker cp` 写入的文件在容器重启/重建后保留。
> 日志在容器内 `/shell/wolgw.log`（compose 方式则直接写回宿主机 `shell/wolgw.log`）。
> 注意：方法二沿用镜像原 entrypoint，旧容器停止时可能需等满 10s 超时；方法一（compose）已解决。

## 使用

1. 浏览器打开 `http://<宿主机IP>`
2. **① BEMFA 全局配置**（所有设备共用）：
   - `uid`：bemfa 控制台 → 设备 → 创建设备后获取的私钥（Client ID）
   - 服务器/端口默认 `bemfa.com:8344`，一般不改
3. **② 目标设备**（点「＋ 添加设备」可加多台，每台一张卡片）：
   - `名称`：显示名，随意
   - `topic`：该设备的 bemfa 主题（**各台必须不同**，是 bemfa 指令的路由依据）
   - `目标IP`：要唤醒/关机的机器的局域网 IP
   - `WOL MAC 地址`：该机器网卡的 MAC（唤醒用，冒号/连字符均可）
   - `广播地址`：默认 `255.255.255.255`，异常时可指定子网广播（如 `192.168.1.255`）
   - `SSH 主机/端口/用户/密码`：执行关机命令用（主机留空则同目标 IP）
   - `关机命令`：默认 `systemctl poweroff`；需要 sudo 就写 `sudo -S poweroff`（程序自动把 SSH 密码喂给 sudo）
4. 点 **保存全部配置**，页面顶部圆点变绿表示 bemfa 已连接（已订阅全部设备 topic）
5. 每台设备卡片上有 **立即唤醒 / 立即关机** 按钮，可单独手动测试

> **旧版单设备配置自动迁移**：`config.json` 是旧版扁平格式（顶层 `topic/targetIp/wolMac`）时，
> 加载时会自动转成「一台设备 + 原 topic」，无需手工修改。
>
> **topic 数量**：bemfa 单条订阅消息最多 8 个 topic，程序自动分批订阅，设备数量不受限。

### 在 bemfa App 中控制（每台设备一个 topic）

在 bemfa 应用里对**某台设备自己的 topic** 直接发送文本指令，网关只对该台设备生效：

| 发送 | 效果 |
|---|---|
| `wake` 或 `on` | WOL 唤醒该设备 |
| `shutdown` 或 `off` | SSH 关机该设备 |
| `status` | 记录当前状态到本地（Web 页面可见，不推送云端） |

> **接收机制说明（重要）**：
> - bemfa 控制台「发送数据」用的是账号自己的密钥，而**同一 uid 发出的消息不会推送回该 uid 的订阅者**（回环抑制）。因此网关不依赖云端推送，改为每 5 秒用 `cmd=9` 主动拉取该 topic 的最新一条消息（延迟 ≤5s），按时间戳去重，不会重复执行。
> - 云端每个 topic 只保留**最新一条**消息：如果网关重启后，云端最新一条是重启前发的旧指令，网关启动后会在第一轮轮询里补执行一次（如补发一个 WOL 包，已开机的机器会忽略）。要执行新指令，请在网关启动后再发送。
> - 网关**不向云端发布任何内容**（不报 online、不发布 wake:ok 等结果），执行结果只在 Web 界面 `http://<宿主机IP>` 查看。

## 网络要求（重要）

- **WOL 唤醒**：魔术包是二层广播，Docker bridge 网络的广播**不会**转发到宿主机 LAN。
  建议容器使用 `--network=host` 运行；若已有容器是 bridge 模式且 WOL 不通，
  需要以 host 网络重建容器（镜像不变，只是启动参数不同）。
  广播地址一栏可填子网广播（如 `192.168.1.255`）或 `255.255.255.255`。

### 跨网段 WOL（网关与目标电脑不在同一网段，如 iKuai 多 LAN / VLAN 场景）

**原理**：`255.255.255.255` 只能到达网关自己所在的二层网段，路由器默认**不转发广播包**。
本版本唤醒时自动发往 3 个目标（去重），日志可见实际发送目标：

```
魔术包已发送 → 255.255.255.255, 192.168.3.255, 192.168.3.9
                └ 配置广播地址   └ /24 网段定向广播(自动)  └ 目标IP直发
```

不通时按以下顺序排查（场景：容器 .7.x → iKuai 路由 → 电脑 .3.x）：

**① 首选：容器改 host 网络**。iKuai Docker 编辑容器 → 网络模式 `host`
（无需端口映射，程序直接监听宿主机 80）。此时魔术包由路由器自己发出、
不经过 iptables FORWARD 规则，`.3.x` 的定向广播必达目标 LAN。

**② 备选：在路由器上配静态 ARP + 走"目标 IP 直发"**。若容器必须保持 bridge 且
路由器的 FORWARD 链拦截了定向广播转发，可借助第 3 个目标（目标 IP 直发）：
电脑关机时路由器 ARP 解析不到 MAC → 丢包；给它配一条**永久静态 ARP** 即可：

```sh
# iKuai CLI / SSH（任意 Linux 路由同理）
ip route get 192.168.3.1   # 先确认 .3.x 网段走哪个网卡（dev ethX）
ip neigh add 192.168.3.<电脑IP> lladdr <电脑MAC> nud permanent dev ethX

# 验证：关机状态下 ping 该 IP，应显示 "from ... dev ethX" 且 ARP 不超时重解析
```

配好后发往 `192.168.3.<IP>` 的魔术包能按静态条目组帧转发。
> **注意**：待机态网卡是否接受"定向单播"魔术包取决于芯片——多数消费级网卡只响应
> 广播/组播。若配了 ARP 仍不醒，说明该网卡只认广播 → 回方案①（host 网络），
> 或把 wolgw 部署到与目标同网段的机器上（NAS、软路由等）。

**③ 兜底：换部署位置**。将 wolgw 跑在一台位于 `.3.x` 网段的主机上，广播天然直达。
- **SSH 关机**：容器需能访问目标主机 22 端口。
- **Web 界面**：宿主机需能访问容器 80 端口（创建容器时映射，或 host 网络）。

## 常见问题

| 现象 | 处理 |
|---|---|
| 顶部一直红点「未连接」 | 检查 uid/topic 是否正确、宿主机能否访问 bemfa.com:8344（`telnet bemfa.com 8344`）；看 `/shell/wolgw.log` |
| 唤醒无反应 | ① 目标网卡需支持 WOL 且 BIOS/系统开启，先用**同网段**机器 `wakeonlan <MAC>` 交叉验证；② 看日志「魔术包已发送 → …」确认实际发送目标；③ 跨网段场景见上文《跨网段 WOL》（host 网络 / 静态 ARP） |
| 关机报错 `sudo: a password is required` | 关机命令改为 `sudo -S poweroff` |
| 关机报错 `connection reset` / `EOF` | 通常表示命令已执行、主机正在断电（正常现象） |
| 80 端口打不开 | `docker port <容器>` 查看映射；或进容器 `cat /shell/wolgw.log` 确认已监听 |

## 重新编译（可选）

```bash
cd wolgw/src
docker run --rm -e GOPROXY=https://goproxy.cn,direct \
  -v "$PWD":/src -v "$PWD/../dist":/out -w /src \
  golang:1.24-alpine \
  sh -c "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/wolgw ."
```
