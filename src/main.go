// wolgw — bemfa 多设备 WOL/关机网关（纯 Go，无 Python 依赖）
//
// 功能：
//   - 通过巴法云 bemfa TCP 协议 (bemfa.com:8344) 订阅多台设备的主题（单条最多 8 个，自动分批）
//   - 云端交互纯接收：订阅 + cmd=9 轮询拉取指令(on/off) + ping 心跳，不向云端推送任何信息
//   - 按 topic 路由指令到对应设备：wake 唤醒 / shutdown 关机 / status 状态（结果仅本地展示）
//   - 心跳保活、断线自动重连、设备状态维护
//   - 每台设备独立配置：目标 IP、WOL MAC、SSH 凭据、关机命令
//   - 内置 Web 界面管理多台设备
package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed static
var staticFS embed.FS

const (
	defBemfaHost = "bemfa.com"
	defBemfaPort = 8344
	defBroadcast = "255.255.255.255"
	defSSHUser   = "root"
	defSSHPort   = 22
	defShutdown  = "systemctl poweroff"
	maxTopics    = 8 // bemfa 单条订阅最多 8 个主题
)

var macRe = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)

// ---------- 配置 ----------

// Device 单台目标设备的完整配置，对应一个 bemfa topic
type Device struct {
	Name        string `json:"name"`
	Topic       string `json:"topic"`
	TargetIP    string `json:"targetIp"`
	WOLMAC      string `json:"wolMac"`
	Broadcast   string `json:"broadcast"`
	SSHHost     string `json:"sshHost"`
	SSHUser     string `json:"sshUser"`
	SSHPassword string `json:"sshPassword"`
	SSHPort     int    `json:"sshPort"`
	ShutdownCmd string `json:"shutdownCmd"`
}

func (d *Device) applyDefaults() {
	if d.Name == "" {
		d.Name = "设备"
	}
	if d.Broadcast == "" {
		d.Broadcast = defBroadcast
	}
	if d.SSHHost == "" {
		d.SSHHost = d.TargetIP
	}
	if d.SSHUser == "" {
		d.SSHUser = defSSHUser
	}
	if d.SSHPort <= 0 {
		d.SSHPort = defSSHPort
	}
	if d.ShutdownCmd == "" {
		d.ShutdownCmd = defShutdown
	}
}

type Config struct {
	BemfaUID  string   `json:"bemfaUid"`
	BemfaHost string   `json:"bemfaHost"`
	BemfaPort int      `json:"bemfaPort"`
	LogLevel  string   `json:"logLevel"` // info | debug（默认 info；debug 时输出心跳等调试日志）
	Devices   []Device `json:"devices"`
}

func (c *Config) applyDefaults() {
	if c.BemfaHost == "" {
		c.BemfaHost = defBemfaHost
	}
	if c.BemfaPort <= 0 {
		c.BemfaPort = defBemfaPort
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	for i := range c.Devices {
		c.Devices[i].applyDefaults()
	}
}

// Validate 校验完整配置合法性（含 topic 唯一性）
func (c *Config) Validate() error {
	if strings.TrimSpace(c.BemfaUID) == "" {
		return fmt.Errorf("bemfa 用户私钥(uid)不能为空")
	}
	if len(c.Devices) == 0 {
		return fmt.Errorf("至少需要添加一台设备")
	}
	if err := c.ValidateDevices(); err != nil {
		return err
	}
	if c.BemfaPort < 1 || c.BemfaPort > 65535 {
		return fmt.Errorf("bemfa 端口不合法")
	}
	return nil
}

// ValidateDevices 仅校验设备列表（topic 唯一/非空、MAC 格式、SSH 端口）
func (c *Config) ValidateDevices() error {
	seen := map[string]bool{}
	for i := range c.Devices {
		d := &c.Devices[i]
		t := strings.TrimSpace(d.Topic)
		if t == "" {
			return fmt.Errorf("设备[%s] 的 topic 不能为空", d.Name)
		}
		if seen[t] {
			return fmt.Errorf("topic[%s] 重复，每台设备必须使用不同 topic", t)
		}
		seen[t] = true
		if d.WOLMAC != "" {
			if _, err := parseMAC(d.WOLMAC); err != nil {
				return fmt.Errorf("设备[%s] MAC 地址格式错误: %v", d.Name, err)
			}
		}
		if p := d.SSHPort; p < 1 || p > 65535 {
			return fmt.Errorf("设备[%s] SSH 端口不合法", d.Name)
		}
	}
	return nil
}

// 旧版单设备扁平配置（用于自动迁移）
type legacyConfig struct {
	Topic       string
	TargetIP    string
	WOLMAC      string
	Broadcast   string
	SSHHost     string
	SSHUser     string
	SSHPassword string
	ShutdownCmd string
	SSHPort     int
}

func loadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.applyDefaults()
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("配置解析失败: %w", err)
	}
	// 兼容旧版单设备格式：顶层有 topic/targetIp 且无 devices
	if len(c.Devices) == 0 {
		var old legacyConfig
		if json.Unmarshal(data, &old) == nil && (old.Topic != "" || old.TargetIP != "" || old.WOLMAC != "") {
			dev := Device{
				Name: "设备1", Topic: old.Topic, TargetIP: old.TargetIP, WOLMAC: old.WOLMAC,
				Broadcast: old.Broadcast, SSHHost: old.SSHHost, SSHUser: old.SSHUser,
				SSHPassword: old.SSHPassword, SSHPort: old.SSHPort, ShutdownCmd: old.ShutdownCmd,
			}
			dev.applyDefaults()
			c.Devices = []Device{dev}
			log.Printf("检测到旧版单设备配置，已自动迁移为设备[设备1]")
		}
	}
	c.applyDefaults()
	return c, nil
}

func saveConfig(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ---------- 运行时状态 ----------

type LogEntry struct {
	Time  string `json:"time"` // yyyy-MM-dd HH:mm:ss
	Level string `json:"level"` // info | debug
	Msg   string `json:"msg"`
}

// DeviceRuntime 单台设备的运行时状态
type DeviceRuntime struct {
	Topic        string `json:"topic"`
	Name         string `json:"name"`
	Busy         string `json:"busy"`
	LastResult   string `json:"lastResult"`
	LastResultAt string `json:"lastResultAt"`
	Online       *bool  `json:"online"` // 设备是否开机（nil = 尚未探测）
}

type Status struct {
	Connected     bool            `json:"connected"`
	Server        string          `json:"server"`
	LastHeartbeat string          `json:"lastHeartbeat"`
	LastError     string          `json:"lastError"`
	Log           []LogEntry      `json:"log"`
	Config        Config          `json:"config"`
	Devices       []DeviceRuntime `json:"devices"`
}

type devState struct {
	busy         string
	lastResult   string
	lastResultAt time.Time
	online       *bool // 目标主机探测结果（nil = 尚未探测）
	cloudTime    int64 // 已处理过的云端消息时间戳（cmd=9 轮询去重）
}

// ---------- 应用 ----------

type App struct {
	cfgPath string

	cfgMu sync.Mutex
	cfg   Config

	connMu sync.Mutex
	conn   net.Conn

	stMu          sync.Mutex
	connected     bool
	lastHeartbeat time.Time
	lastError     string
	devStates     map[string]*devState // 按 topic
	logBuf        []LogEntry
}

func newApp(cfgPath string) *App {
	return &App{cfgPath: cfgPath, devStates: map[string]*devState{}}
}

// log info 级日志（始终输出到容器日志与 Web 日志页）
func (a *App) log(msg string) {
	a.writeLog("info", msg)
}

// logDebug debug 级日志（仅当日志等级为 debug 时输出，如心跳）
func (a *App) logDebug(msg string) {
	a.cfgMu.Lock()
	lv := a.cfg.LogLevel
	a.cfgMu.Unlock()
	if lv != "debug" {
		return
	}
	a.writeLog("debug", msg)
}

// writeLog 写一条日志：stdout（iKuai 日志中心/容器日志可见）+ 内存环形缓冲（Web 日志页）
func (a *App) writeLog(level, msg string) {
	log.Println(msg)
	a.stMu.Lock()
	a.logBuf = append(a.logBuf, LogEntry{Time: time.Now().Format("2006-01-02 15:04:05"), Level: level, Msg: msg})
	if len(a.logBuf) > 500 {
		a.logBuf = a.logBuf[len(a.logBuf)-500:]
	}
	a.stMu.Unlock()
}

func (a *App) loadConfig() Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg
}

// findDevice 按 topic 查找设备
func (a *App) findDevice(topic string) (Device, bool) {
	cfg := a.loadConfig()
	for _, d := range cfg.Devices {
		if d.Topic == topic {
			return d, true
		}
	}
	return Device{}, false
}

func (a *App) setState(topic, busy string) bool {
	a.stMu.Lock()
	defer a.stMu.Unlock()
	st, ok := a.devStates[topic]
	if !ok {
		st = &devState{}
		a.devStates[topic] = st
	}
	if st.busy != "" {
		return false
	}
	st.busy = busy
	return true
}

func (a *App) clearState(topic string) {
	a.stMu.Lock()
	if st, ok := a.devStates[topic]; ok {
		st.busy = ""
	}
	a.stMu.Unlock()
}

func (a *App) setLastResult(topic, res string) {
	a.stMu.Lock()
	st, ok := a.devStates[topic]
	if !ok {
		st = &devState{}
		a.devStates[topic] = st
	}
	st.lastResult = res
	st.lastResultAt = time.Now()
	a.stMu.Unlock()
}

func (a *App) status() Status {
	cfg := a.loadConfig()
	masked := cfg
	masked.Devices = make([]Device, len(cfg.Devices))
	copy(masked.Devices, cfg.Devices) // 深拷贝，避免脱敏污染内存中的真实密码
	for i := range masked.Devices {
		masked.Devices[i].SSHPassword = maskPwd(masked.Devices[i].SSHPassword)
	}
	a.stMu.Lock()
	s := Status{
		Connected:     a.connected,
		LastError:     a.lastError,
		Log:           append([]LogEntry{}, a.logBuf...),
		Config:        masked,
		Devices:       make([]DeviceRuntime, 0, len(cfg.Devices)),
	}
	if !a.lastHeartbeat.IsZero() {
		s.LastHeartbeat = a.lastHeartbeat.Format("15:04:05")
	}
	s.Server = net.JoinHostPort(cfg.BemfaHost, strconv.Itoa(cfg.BemfaPort))
	for _, d := range cfg.Devices {
		dr := DeviceRuntime{Topic: d.Topic, Name: d.Name}
		if st, ok := a.devStates[d.Topic]; ok {
			dr.Busy = st.busy
			dr.LastResult = st.lastResult
			dr.Online = st.online
			if !st.lastResultAt.IsZero() {
				dr.LastResultAt = st.lastResultAt.Format("15:04:05")
			}
		}
		s.Devices = append(s.Devices, dr)
	}
	a.stMu.Unlock()
	return s
}

// ---------- bemfa TCP 客户端 ----------

func (a *App) writeBemfa(data string) error {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("bemfa 未连接")
	}
	a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := a.conn.Write([]byte(data))
	return err
}

// runBemfa 主循环：连接 → 订阅 → 收消息，断线重连
func (a *App) runBemfa() {
	backoff := 2 * time.Second
	for {
		cfg := a.loadConfig()
		if strings.TrimSpace(cfg.BemfaUID) == "" || len(cfg.Devices) == 0 {
			a.log("bemfa 未配置 uid 或设备，等待配置…")
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				if newCfg := a.loadConfig(); newCfg.BemfaUID != "" && len(newCfg.Devices) > 0 {
					break
				}
			}
			continue
		}

		addr := fmt.Sprintf("%s:%d", cfg.BemfaHost, cfg.BemfaPort)
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			a.log(fmt.Sprintf("bemfa 连接失败 %s: %v，%v 后重试", addr, err, backoff))
			a.stMu.Lock()
			a.connected = false
			a.lastError = err.Error()
			a.stMu.Unlock()
			sleepWithCheck(a, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second
		a.connMu.Lock()
		a.conn = conn
		a.connMu.Unlock()
		a.stMu.Lock()
		a.connected = true
		a.lastError = ""
		a.stMu.Unlock()

		// 订阅所有设备主题（每条最多 8 个）
		topics := make([]string, 0, len(cfg.Devices))
		for _, d := range cfg.Devices {
			topics = append(topics, d.Topic)
		}
		subOK := true
		for i := 0; i < len(topics); i += maxTopics {
			end := i + maxTopics
			if end > len(topics) {
				end = len(topics)
			}
			line := fmt.Sprintf("cmd=1&uid=%s&topic=%s\r\n", cfg.BemfaUID, strings.Join(topics[i:end], ","))
			if err := a.writeBemfa(line); err != nil {
				subOK = false
				break
			}
		}
		if subOK {
			a.log(fmt.Sprintf("bemfa 已连接 %s，订阅 %d 个设备主题: %s", addr, len(topics), strings.Join(topics, ", ")))
		}

		// 心跳 + 读消息
		dead := make(chan struct{})
		go func() {
			ticker := time.NewTicker(45 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-dead:
					return
				case <-ticker.C:
					if a.writeBemfa("ping\r\n") != nil {
						select {
						case <-dead:
						default:
							conn.Close()
						}
						return
					}
					a.stMu.Lock()
					a.lastHeartbeat = time.Now()
					a.stMu.Unlock()
					a.logDebug("心跳: ping 已发送 (每 45s)")
				}
			}
		}()

		// 云端状态轮询：控制台同密钥发送会被回环拦截，用 cmd=9 定期拉取最新一条消息
		pollTicker := time.NewTicker(5 * time.Second)
		go func() {
			defer pollTicker.Stop()
			for {
				select {
				case <-dead:
					return
				case <-pollTicker.C:
					pc := a.loadConfig()
					for _, d := range pc.Devices {
						if a.writeBemfa(fmt.Sprintf("cmd=9&uid=%s&topic=%s\r\n", pc.BemfaUID, d.Topic)) != nil {
							return
						}
					}
					a.logDebug(fmt.Sprintf("指令轮询: cmd=9 拉取 %d 个主题 (每 5s)", len(pc.Devices)))
				}
			}
		}()

		reader := bufio.NewReader(conn)
		loop := true
		for loop {
			line, err := reader.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if line != "" {
				a.handleLine(line)
			}
			if err != nil {
				a.log("bemfa 连接断开: " + err.Error())
				loop = false
			}
		}
		close(dead)
		a.connMu.Lock()
		a.conn = nil
		a.connMu.Unlock()
		a.stMu.Lock()
		a.connected = false
		a.stMu.Unlock()
		time.Sleep(1 * time.Second)
	}
}

// handleLine 解析一条 bemfa 协议消息
func (a *App) handleLine(line string) {
	f := parseKV(line)
	switch f["cmd"] {
	case "0": // 心跳应答
		if f["res"] == "1" {
			a.stMu.Lock()
			a.lastHeartbeat = time.Now()
			a.stMu.Unlock()
			a.logDebug("心跳: bemfa 应答正常 (res=1)")
		}
	case "1": // 订阅应答 或 订阅主题的消息推送
		if f["msg"] != "" {
			a.handleCommand(f["topic"], f["msg"])
		} else if f["res"] != "" && f["res"] != "1" {
			a.log(fmt.Sprintf("bemfa 订阅失败(res=%s)，请检查 uid 是否正确", f["res"]))
		}
	case "2": // 发布应答
		if f["res"] != "" && f["res"] != "1" {
			a.log("bemfa 发布失败(res=" + f["res"] + ")")
		}
	case "9": // 云端最新一条消息（轮询拉取）
		t, _ := strconv.ParseInt(f["time"], 10, 64)
		msg := f["msg"]
		if u, err := url.QueryUnescape(msg); err == nil {
			msg = u
		}
		a.onCloudMsg(f["topic"], t, msg)
	default:
		if line != "" {
			a.log("bemfa 其他消息: " + line)
		}
	}
}

func parseKV(line string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(line, "&") {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// onCloudMsg 处理 cmd=9 轮询拉取到的云端最新消息
func (a *App) onCloudMsg(topic string, cloudTime int64, msg string) {
	a.stMu.Lock()
	st, ok := a.devStates[topic]
	if !ok {
		st = &devState{}
		a.devStates[topic] = st
	}
	if cloudTime <= st.cloudTime {
		a.stMu.Unlock()
		return
	}
	st.cloudTime = cloudTime
	a.stMu.Unlock()
	if msg == "" {
		return // 空消息忽略
	}
	a.handleCommand(topic, msg)
}

// handleCommand 按 topic 路由指令到对应设备
func (a *App) handleCommand(topic, msg string) {
	dev, ok := a.findDevice(topic)
	if !ok {
		// 兼容推送主题带 /set 后缀
		dev, ok = a.findDevice(strings.TrimSuffix(topic, "/set"))
	}
	if !ok {
		a.log(fmt.Sprintf("收到未知主题[%s]的指令: %s", topic, msg))
		return
	}
	a.log(fmt.Sprintf("设备[%s] 收到指令: %s", dev.Topic, msg))
	switch strings.ToLower(strings.TrimSpace(msg)) {
	case "wake", "on", "1", "开机", "唤醒":
		a.execWake(dev, "bemfa")
	case "shutdown", "off", "0", "关机":
		a.execShutdown(dev, "bemfa")
	case "status":
		online := "offline"
		a.stMu.Lock()
		if a.connected {
			online = "online"
		}
		a.stMu.Unlock()
		a.setLastResult(dev.Topic, "gateway="+online+" target="+dev.TargetIP)
		a.log(fmt.Sprintf("设备[%s] 状态查询: gateway=%s target=%s（仅本地记录，不推送云端）", dev.Topic, online, dev.TargetIP))
	default:
		a.log(fmt.Sprintf("设备[%s] 未知指令: %q（支持 wake/shutdown/status）", dev.Topic, msg))
	}
}

// ---------- WOL ----------

func parseMAC(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	clean := strings.NewReplacer(":", "", "-", "").Replace(s)
	if len(clean) != 12 {
		return nil, fmt.Errorf("MAC 应为 12 位十六进制，当前: %s", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("MAC 含非法字符: %s", s)
	}
	return b, nil
}

// sendMagic 发送 WOL 魔术包
func sendMagic(mac []byte, dst string) error {
	pkt := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, mac...)
	}
	udpAddr, err := net.ResolveUDPAddr("udp", dst+":9")
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	if c, err := conn.SyscallConn(); err == nil {
		_ = c.Control(func(fd uintptr) {
			setBroadcast(fd)
		})
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (a *App) execWake(dev Device, source string) string {
	if !a.setState(dev.Topic, "wake") {
		res := "wake:busy 该设备正在执行其他操作"
		a.log(res)
		return res
	}
	defer a.clearState(dev.Topic)

	mac, err := parseMAC(dev.WOLMAC)
	if err != nil {
		res := "wake:err " + err.Error()
		a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
		a.report(dev.Topic, res)
		return res
	}
	// 发送目标：广播地址 + 网段定向广播(按 /24 自动推导) + 目标 IP（去重、去空）
	targets := []string{}
	seen := map[string]bool{}
	subnetBC := ""
	if ip := net.ParseIP(strings.TrimSpace(dev.TargetIP)); ip != nil {
		if v4 := ip.To4(); v4 != nil && v4[3] != 255 {
			subnetBC = fmt.Sprintf("%d.%d.%d.255", v4[0], v4[1], v4[2]) // 跨网段 WOL：定向广播交由路由器转发
		}
	}
	for _, t := range []string{dev.Broadcast, subnetBC, dev.TargetIP} {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	var errs []string
	for _, t := range targets {
		if err := sendMagic(mac, t); err != nil {
			errs = append(errs, t+": "+err.Error())
		}
	}
	res := "wake:ok 魔术包已发送 → " + strings.Join(targets, ", ")
	if len(errs) == len(targets) {
		res = "wake:err " + strings.Join(errs, " | ")
	} else if len(errs) > 0 {
		res += "（部分失败: " + strings.Join(errs, " | ") + "）"
	}
	a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
	a.report(dev.Topic, res)
	return res
}

// ---------- SSH 关机 ----------

func (a *App) execShutdown(dev Device, source string) string {
	if !a.setState(dev.Topic, "shutdown") {
		res := "shutdown:busy 该设备正在执行其他操作"
		a.log(res)
		return res
	}
	defer a.clearState(dev.Topic)

	host := dev.SSHHost
	if host == "" {
		host = dev.TargetIP
	}
	addr := net.JoinHostPort(host, strconv.Itoa(dev.SSHPort))
	cliCfg := &ssh.ClientConfig{
		User:            dev.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.Password(dev.SSHPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, cliCfg)
	if err != nil {
		res := "shutdown:err 无法连接 " + addr + ": " + err.Error() + "（目标可能已关机）"
		a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
		a.report(dev.Topic, res)
		return res
	}
	defer conn.Close()
	session, err := conn.NewSession()
	if err != nil {
		res := "shutdown:err 创建会话失败: " + err.Error()
		a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
		a.report(dev.Topic, res)
		return res
	}
	defer session.Close()

	var out bytes.Buffer
	session.Stdout = &out
	session.Stderr = &out
	// 命令需要 sudo 时，把密码喂给 sudo -S
	if strings.Contains(dev.ShutdownCmd, "sudo") {
		stdin, _ := session.StdinPipe()
		go func() {
			stdin.Write([]byte(dev.SSHPassword + "\n"))
			stdin.Close()
		}()
	}
	err = session.Start(dev.ShutdownCmd)
	if err != nil {
		res := "shutdown:err 执行命令失败: " + err.Error()
		a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
		a.report(dev.Topic, res)
		return res
	}
	waitErr := session.Wait()
	o := tail(out.String(), 200)
	res := "shutdown:ok 命令已执行: " + dev.ShutdownCmd
	if waitErr != nil {
		// 关机过程中连接被对端关闭属于正常现象
		msg := waitErr.Error()
		if strings.Contains(msg, "Connection closed") || strings.Contains(msg, "Connection reset") || msg == "EOF" {
			res += "（主机已断开，正常）"
			if o != "" {
				res += " | " + o
			}
		} else {
			res = "shutdown:err " + msg
			if o != "" {
				res += " | 输出: " + o
			}
		}
	} else if o != "" {
		res += " | 输出: " + o
	}
	a.log(fmt.Sprintf("设备[%s] %s (来源:%s)", dev.Topic, res, source))
	a.report(dev.Topic, res)
	return res
}

// report 记录执行结果（仅本地 Web/API 展示，不向云端推送）
func (a *App) report(topic, res string) {
	a.setLastResult(topic, res)
}

// ---------- 设备在线探测 ----------

// checkHost 探测主机是否开机：TCP 连接 80/22 端口，
// 连上或立即拒绝(端口关闭) = 开机；全部超时 = 关机/不可达。无需 root 权限。
func checkHost(ip string) bool {
	for _, port := range []int{80, 22} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 1200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		if connRefused(err) {
			return true // 主机在线、端口关闭
		}
	}
	return false
}

// runPinger 每 15 秒探测一次各设备目标主机是否开机
func (a *App) runPinger() {
	check := func() {
		cfg := a.loadConfig()
		var wg sync.WaitGroup
		for _, d := range cfg.Devices {
			ip := strings.TrimSpace(d.TargetIP)
			if ip == "" {
				continue
			}
			wg.Add(1)
			go func(topic, ip string) {
				defer wg.Done()
				online := checkHost(ip)
				a.stMu.Lock()
				st, ok := a.devStates[topic]
				if !ok {
					st = &devState{}
					a.devStates[topic] = st
				}
				st.online = &online
				a.stMu.Unlock()
			}(d.Topic, ip)
		}
		wg.Wait()
	}
	check()
	t := time.NewTicker(15 * time.Second)
	for range t.C {
		check()
	}
}

func (a *App) sleepUntil(until time.Time) {
	for time.Now().Before(until) {
		time.Sleep(200 * time.Millisecond)
	}
}

func sleepWithCheck(a *App, d time.Duration) {
	a.sleepUntil(time.Now().Add(d))
}

// maskPwd 密码脱敏：保留首尾字符，中间打码
func maskPwd(p string) string {
	if len(p) == 0 {
		return ""
	}
	if len(p) < 3 {
		return strings.Repeat("*", len(p))
	}
	return p[:1] + strings.Repeat("*", len(p)-2) + p[len(p)-1:]
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------- Web ----------

type apiResult struct {
	OK     bool        `json:"ok"`
	Result string      `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
	Status interface{} `json:"status,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *App) handlerStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.status())
}

func (a *App) handlerSaveConfig(w http.ResponseWriter, r *http.Request) {
	var c Config
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, 400, apiResult{Error: "请求体解析失败: " + err.Error()})
		return
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		writeJSON(w, 400, apiResult{Error: err.Error()})
		return
	}
	// 脱敏保护：提交值 == 脱敏值 → 保持原密码（防止打码串写回配置）
	old := a.loadConfig()
	oldPwd := map[string]string{}
	for _, d := range old.Devices {
		oldPwd[d.Topic] = d.SSHPassword
	}
	for i := range c.Devices {
		if o, ok := oldPwd[c.Devices[i].Topic]; ok && o != "" && c.Devices[i].SSHPassword == maskPwd(o) {
			c.Devices[i].SSHPassword = o
		}
	}
	if err := saveConfig(a.cfgPath, c); err != nil {
		writeJSON(w, 500, apiResult{Error: "配置保存失败: " + err.Error()})
		return
	}
	a.cfgMu.Lock()
	a.cfg = c
	a.cfgMu.Unlock()
	a.log(fmt.Sprintf("配置已保存（%d 台设备）", len(c.Devices)))
	a.bemfaForceReconnect()
	writeJSON(w, 200, apiResult{OK: true, Status: a.status()})
}

// 保存巴法云配置（仅 uid/host/port，不动设备列表）
func (a *App) handlerSaveBemfa(w http.ResponseWriter, r *http.Request) {
	var b struct {
		BemfaUID  string `json:"bemfaUid"`
		BemfaHost string `json:"bemfaHost"`
		BemfaPort int    `json:"bemfaPort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, apiResult{Error: "请求体解析失败: " + err.Error()})
		return
	}
	if b.BemfaPort < 1 || b.BemfaPort > 65535 {
		writeJSON(w, 400, apiResult{Error: "bemfa 端口不合法（1-65535）"})
		return
	}
	a.cfgMu.Lock()
	c := a.cfg
	c.BemfaUID, c.BemfaHost, c.BemfaPort = b.BemfaUID, b.BemfaHost, b.BemfaPort
	a.cfgMu.Unlock()
	if err := saveConfig(a.cfgPath, c); err != nil {
		writeJSON(w, 500, apiResult{Error: "配置保存失败: " + err.Error()})
		return
	}
	a.cfgMu.Lock()
	a.cfg = c
	a.cfgMu.Unlock()
	a.log("巴法云配置已保存")
	a.bemfaForceReconnect()
	writeJSON(w, 200, apiResult{OK: true, Status: a.status()})
}

// 保存设备列表（整体替换；空密码 = 保留原密码）
func (a *App) handlerSaveDevices(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Devices []Device `json:"devices"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, apiResult{Error: "请求体解析失败: " + err.Error()})
		return
	}
	old := a.loadConfig()
	// 空密码 → 沿用旧配置中同 topic 设备的密码
	oldPwd := map[string]string{}
	for _, d := range old.Devices {
		oldPwd[d.Topic] = d.SSHPassword
	}
	for i := range b.Devices {
		old := oldPwd[b.Devices[i].Topic]
		if b.Devices[i].SSHPassword == "" {
			b.Devices[i].SSHPassword = old
		} else if old != "" && b.Devices[i].SSHPassword == maskPwd(old) {
			// 提交的是脱敏值（表单未改动密码）→ 保持原密码，防止打码串写回配置
			b.Devices[i].SSHPassword = old
		}
	}
	a.cfgMu.Lock()
	c := a.cfg
	c.Devices = b.Devices
	c.applyDefaults()
	a.cfgMu.Unlock()
	if err := c.ValidateDevices(); err != nil {
		writeJSON(w, 400, apiResult{Error: err.Error()})
		return
	}
	if err := saveConfig(a.cfgPath, c); err != nil {
		writeJSON(w, 500, apiResult{Error: "配置保存失败: " + err.Error()})
		return
	}
	a.cfgMu.Lock()
	a.cfg = c
	a.cfgMu.Unlock()
	a.log(fmt.Sprintf("设备列表已保存（%d 台设备）", len(c.Devices)))
	a.bemfaForceReconnect()
	writeJSON(w, 200, apiResult{OK: true, Status: a.status()})
}

// 切换日志等级（持久化到配置文件，重启依然有效；debug 时输出心跳等调试日志）
// GET /api/pwd?topic=xxx —— 按需获取设备真实 SSH 密码（用户在页面请求"显示明文"时调用）
func (a *App) handlerGetPwd(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		writeJSON(w, 400, apiResult{Error: "缺少 topic"})
		return
	}
	cfg := a.loadConfig()
	for _, d := range cfg.Devices {
		if d.Topic == topic {
			writeJSON(w, 200, map[string]string{"password": d.SSHPassword})
			return
		}
	}
	writeJSON(w, 404, apiResult{Error: "设备不存在"})
}

func (a *App) handlerSetLogLevel(w http.ResponseWriter, r *http.Request) {
	var b struct{ Level string `json:"level"` }
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, apiResult{Error: "请求体解析失败: " + err.Error()})
		return
	}
	if b.Level != "info" && b.Level != "debug" {
		writeJSON(w, 400, apiResult{Error: "日志等级不合法（仅支持 info / debug）"})
		return
	}
	a.cfgMu.Lock()
	c := a.cfg
	c.LogLevel = b.Level
	a.cfgMu.Unlock()
	if err := saveConfig(a.cfgPath, c); err != nil {
		writeJSON(w, 500, apiResult{Error: "配置保存失败: " + err.Error()})
		return
	}
	a.cfgMu.Lock()
	a.cfg = c
	a.cfgMu.Unlock()
	a.log(fmt.Sprintf("日志等级已切换为 %s（已持久化，重启依然有效）", b.Level))
	writeJSON(w, 200, apiResult{OK: true, Status: a.status()})
}

// 日志列表（供 Web 日志页）
func (a *App) handlerLogs(w http.ResponseWriter, r *http.Request) {
	a.stMu.Lock()
	entries := append([]LogEntry{}, a.logBuf...)
	a.stMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"log": entries})
}

// 保存配置后强制 bemfa 重连（使新主题生效）
func (a *App) bemfaForceReconnect() {
	a.connMu.Lock()
	if a.conn != nil {
		a.conn.Close()
	}
	a.connMu.Unlock()
}

func (a *App) handlerWake(w http.ResponseWriter, r *http.Request) {
	var q struct{ Topic string `json:"topic"` }
	json.NewDecoder(r.Body).Decode(&q)
	dev, ok := a.findDevice(q.Topic)
	if !ok {
		writeJSON(w, 404, apiResult{Error: "找不到 topic 对应的设备: " + q.Topic})
		return
	}
	res := a.execWake(dev, "web")
	writeJSON(w, 200, apiResult{OK: strings.HasPrefix(res, "wake:ok"), Result: res, Status: a.status()})
}

func (a *App) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	var q struct{ Topic string `json:"topic"` }
	json.NewDecoder(r.Body).Decode(&q)
	dev, ok := a.findDevice(q.Topic)
	if !ok {
		writeJSON(w, 404, apiResult{Error: "找不到 topic 对应的设备: " + q.Topic})
		return
	}
	res := a.execShutdown(dev, "web")
	writeJSON(w, 200, apiResult{OK: strings.HasPrefix(res, "shutdown:ok"), Result: res, Status: a.status()})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/status", a.handlerStatus)
	mux.HandleFunc("POST /api/config", a.handlerSaveConfig)
	mux.HandleFunc("POST /api/bemfa", a.handlerSaveBemfa)
	mux.HandleFunc("POST /api/devices", a.handlerSaveDevices)
	mux.HandleFunc("GET /api/pwd", a.handlerGetPwd)
	mux.HandleFunc("POST /api/loglevel", a.handlerSetLogLevel)
	mux.HandleFunc("GET /api/logs", a.handlerLogs)
	mux.HandleFunc("POST /api/wake", a.handlerWake)
	mux.HandleFunc("POST /api/shutdown", a.handlerShutdown)
	return mux
}

// ---------- main ----------

func main() {
	cfgPath := flag.String("config", "/shell/config.json", "配置文件路径")
	listen := flag.String("listen", ":80", "Web 监听地址")
	flag.Parse()

	app := newApp(*cfgPath)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Printf("读取配置失败(%v)，使用默认配置", err)
	}
	app.cfgMu.Lock()
	app.cfg = cfg
	app.cfgMu.Unlock()
	app.log(fmt.Sprintf("wolgw 启动，配置 %d 台设备", len(cfg.Devices)))

	go app.runBemfa()
	go app.runPinger()

	log.Printf("Web 界面: http://<本机IP>%s", *listen)
	if err := http.ListenAndServe(*listen, app.routes()); err != nil {
		log.Fatalf("Web 服务退出: %v", err)
	}
}
