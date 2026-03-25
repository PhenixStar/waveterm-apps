// wt-dashboard-web: Wave Terminal web dashboard
//
// Build:  go build -o bin/wt-dashboard-web.exe ./cmd/dashboard-web/
// Run:    bin/wt-dashboard-web.exe [path/to/machines.json]
//
// Widget entry (widgets.json):
//   "blockdef": { "meta": { "view": "web", "url": "http://localhost:9800",
//     "pinnedurl": "http://localhost:9800", "web:hidenav": true } }
package main

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec -- WebSocket RFC 6455 requires SHA-1
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	gossh "golang.org/x/crypto/ssh"
)

//go:embed index.html
var indexHTML []byte

// ============================================================================
// Config (shared types mirrored from cmd/dashboard)
// ============================================================================

type MachineType string

const (
	TypeLinux    MachineType = "linux"
	TypeWindows  MachineType = "windows"
	TypeMikroTik MachineType = "mikrotik"
)

type MachineConfig struct {
	Name     string      `json:"name"`
	Host     string      `json:"host"`
	Port     int         `json:"port"`
	User     string      `json:"user"`
	Type     MachineType `json:"type"`
	KeyPath  string      `json:"keyPath"`
	Password string      `json:"password,omitempty"`
	Comment  string      `json:"comment,omitempty"`
}

type Config struct {
	PollIntervalSeconds int             `json:"pollIntervalSeconds"`
	SSHTimeoutSeconds   int             `json:"sshTimeoutSeconds"`
	Machines            []MachineConfig `json:"machines"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 15
	}
	if cfg.SSHTimeoutSeconds <= 0 {
		cfg.SSHTimeoutSeconds = 10
	}
	return &cfg, nil
}

// ============================================================================
// Data model
// ============================================================================

type HealthStatus int

const (
	HealthOK HealthStatus = iota
	HealthWarning
	HealthCritical
	HealthUnknown
)

type GPUInfo struct {
	Index       int     `json:"Index"`
	Name        string  `json:"Name"`
	UtilPercent float64 `json:"UtilPercent"`
	MemUsed     uint64  `json:"MemUsed"`
	MemTotal    uint64  `json:"MemTotal"`
	MemPercent  float64 `json:"MemPercent"`
	TempC       float64 `json:"TempC"`
}

type DockerInfo struct {
	Running int `json:"Running"`
	Total   int `json:"Total"`
}

type MikroTikInfo struct {
	WiFiClients int    `json:"WiFiClients"`
	ETHTx       uint64 `json:"ETHTx"`
	ETHRx       uint64 `json:"ETHRx"`
}

type MachineStatus struct {
	Name          string       `json:"Name"`
	Host          string       `json:"Host"`
	Port          int          `json:"Port"`
	User          string       `json:"User"`
	Type          MachineType  `json:"Type"`
	Status        HealthStatus `json:"Status"`
	LastUpdate    time.Time    `json:"LastUpdate"`
	LastError     string       `json:"LastError"`
	CPU           float64      `json:"CPU"`
	MemoryPercent float64      `json:"MemoryPercent"`
	MemoryUsed    uint64       `json:"MemoryUsed"`
	MemoryTotal   uint64       `json:"MemoryTotal"`
	DiskPercent   float64      `json:"DiskPercent"`
	DiskUsed      uint64       `json:"DiskUsed"`
	DiskTotal     uint64       `json:"DiskTotal"`
	UptimeSeconds int64        `json:"UptimeSeconds"`
	GPUs          []GPUInfo    `json:"GPUs"`
	Docker        *DockerInfo  `json:"Docker"`
	MikroTik      *MikroTikInfo `json:"MikroTik"`
}

// ============================================================================
// Health scoring
// ============================================================================

type Thresholds struct {
	CPUWarn, CPUCrit   float64
	MemWarn, MemCrit   float64
	DiskWarn, DiskCrit float64
}

var defaultThresholds = &Thresholds{
	CPUWarn: 80, CPUCrit: 95,
	MemWarn: 85, MemCrit: 95,
	DiskWarn: 80, DiskCrit: 90,
}

func scoreHealth(s *MachineStatus, t *Thresholds) HealthStatus {
	if s.LastError != "" && s.CPU == 0 && s.MemoryPercent == 0 {
		return HealthUnknown
	}
	thresh := func(v, warn, crit float64) HealthStatus {
		switch {
		case v >= crit:
			return HealthCritical
		case v >= warn:
			return HealthWarning
		default:
			return HealthOK
		}
	}
	worst := HealthOK
	bump := func(h HealthStatus) {
		if h > worst {
			worst = h
		}
	}
	bump(thresh(s.CPU, t.CPUWarn, t.CPUCrit))
	bump(thresh(s.MemoryPercent, t.MemWarn, t.MemCrit))
	bump(thresh(s.DiskPercent, t.DiskWarn, t.DiskCrit))
	for _, g := range s.GPUs {
		bump(thresh(g.UtilPercent, t.CPUWarn, t.CPUCrit))
		bump(thresh(g.MemPercent, t.MemWarn, t.MemCrit))
	}
	return worst
}

// ============================================================================
// SSH connection pool
// ============================================================================

type sshPool struct {
	mu      sync.Mutex
	clients map[string]*gossh.Client
	timeout time.Duration
}

func newSSHPool(timeoutSecs int) *sshPool {
	return &sshPool{
		clients: make(map[string]*gossh.Client),
		timeout: time.Duration(timeoutSecs) * time.Second,
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func (p *sshPool) connect(m MachineConfig) (*gossh.Client, error) {
	key := fmt.Sprintf("%s@%s:%d", m.User, m.Host, m.Port)

	// Hold the lock for the full read-check-reconnect cycle to prevent TOCTOU
	// races when multiple goroutines connect to the same host concurrently.
	// SSH dial timeout (p.timeout) bounds the maximum lock hold time.
	p.mu.Lock()
	defer p.mu.Unlock()

	c := p.clients[key]
	if c != nil {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return c, nil
		}
		c.Close()
		delete(p.clients, key)
	}

	var auths []gossh.AuthMethod
	if m.KeyPath != "" {
		keyData, err := os.ReadFile(expandHome(m.KeyPath))
		if err == nil {
			if signer, err := gossh.ParsePrivateKey(keyData); err == nil {
				auths = append(auths, gossh.PublicKeys(signer))
			}
		}
	}
	if m.Password != "" {
		auths = append(auths, gossh.Password(m.Password))
	}
	if len(auths) == 0 {
		auths = append(auths, gossh.Password(""))
	}

	sshCfg := &gossh.ClientConfig{
		User: m.User,
		Auth: auths,
		// InsecureIgnoreHostKey is intentional: these are internal lab hosts
		// with no known_hosts entries. Do not change for production deployments.
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         p.timeout,
	}

	addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
	conn, err := gossh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	p.clients[key] = conn
	return conn, nil
}

func runSSH(client *gossh.Client, cmd string) string {
	sess, err := client.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()
	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Run(cmd) //nolint:errcheck -- non-zero exits are expected
	return strings.TrimSpace(buf.String())
}

func (p *sshPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.Close()
	}
	p.clients = make(map[string]*gossh.Client)
}

// ============================================================================
// Linux collector
// ============================================================================

func collectLinux(pool *sshPool, m MachineConfig) *MachineStatus {
	s := &MachineStatus{
		Name: m.Name, Host: m.Host, Port: m.Port,
		User: m.User, Type: m.Type, LastUpdate: time.Now(),
	}

	client, err := pool.connect(m)
	if err != nil {
		s.LastError = err.Error()
		return s
	}

	if cpu, err := linuxCPU(client); err != nil {
		s.LastError = fmt.Sprintf("cpu: %v", err)
	} else {
		s.CPU = cpu
	}

	s.MemoryUsed, s.MemoryTotal = linuxMemory(client)
	if s.MemoryTotal > 0 {
		s.MemoryPercent = float64(s.MemoryUsed) / float64(s.MemoryTotal) * 100
	}

	s.DiskUsed, s.DiskTotal = linuxDisk(client)
	if s.DiskTotal > 0 {
		s.DiskPercent = float64(s.DiskUsed) / float64(s.DiskTotal) * 100
	}

	if fields := strings.Fields(runSSH(client, "cat /proc/uptime")); len(fields) > 0 {
		if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
			s.UptimeSeconds = int64(f)
		}
	}

	s.GPUs = linuxGPU(client)
	s.Docker = linuxDocker(client)
	return s
}

func readProcStat(client *gossh.Client) (total, idle uint64, err error) {
	out := runSSH(client, "head -1 /proc/stat")
	fields := strings.Fields(out)
	if len(fields) < 5 {
		return 0, 0, fmt.Errorf("unexpected /proc/stat: %q", out)
	}
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 {
			idle = v
		}
	}
	return total, idle, nil
}

func linuxCPU(client *gossh.Client) (float64, error) {
	t1, i1, err := readProcStat(client)
	if err != nil {
		return 0, err
	}
	time.Sleep(500 * time.Millisecond)
	t2, i2, err := readProcStat(client)
	if err != nil {
		return 0, err
	}
	dt := float64(t2 - t1)
	if dt == 0 {
		return 0, nil
	}
	return math.Round((1-float64(i2-i1)/dt)*100*10) / 10, nil
}

func linuxMemory(client *gossh.Client) (used, total uint64) {
	out := runSSH(client, "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo")
	var memTotal, memAvail uint64
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		v *= 1024
		switch fields[0] {
		case "MemTotal:":
			memTotal = v
		case "MemAvailable:":
			memAvail = v
		}
	}
	return memTotal - memAvail, memTotal
}

func linuxDisk(client *gossh.Client) (used, total uint64) {
	out := runSSH(client, "df -B1 / | tail -1")
	fields := strings.Fields(out)
	if len(fields) < 4 {
		return 0, 0
	}
	total, _ = strconv.ParseUint(fields[1], 10, 64)
	used, _ = strconv.ParseUint(fields[2], 10, 64)
	return used, total
}

func linuxGPU(client *gossh.Client) []GPUInfo {
	out := runSSH(client,
		"nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits 2>/dev/null")
	if out == "" {
		return nil
	}
	var gpus []GPUInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), ", ")
		if len(parts) < 6 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		util, _ := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		memUsedMiB, _ := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 64)
		memTotalMiB, _ := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
		temp, _ := strconv.ParseFloat(strings.TrimSpace(parts[5]), 64)
		memUsed := memUsedMiB * 1024 * 1024
		memTotal := memTotalMiB * 1024 * 1024
		var memPct float64
		if memTotal > 0 {
			memPct = float64(memUsed) / float64(memTotal) * 100
		}
		gpus = append(gpus, GPUInfo{
			Index: idx, Name: strings.TrimSpace(parts[1]),
			UtilPercent: util, MemUsed: memUsed, MemTotal: memTotal,
			MemPercent: memPct, TempC: temp,
		})
	}
	return gpus
}

func linuxDocker(client *gossh.Client) *DockerInfo {
	running := runSSH(client, "docker ps -q 2>/dev/null | wc -l")
	if running == "" {
		return nil
	}
	total := runSSH(client, "docker ps -aq 2>/dev/null | wc -l")
	r, _ := strconv.Atoi(running)
	t, _ := strconv.Atoi(total)
	return &DockerInfo{Running: r, Total: t}
}

// ============================================================================
// Windows collector (localhost PowerShell)
// ============================================================================

func collectWindows(m MachineConfig) *MachineStatus {
	s := &MachineStatus{
		Name: m.Name, Host: m.Host, Port: m.Port,
		User: m.User, Type: m.Type, LastUpdate: time.Now(),
	}

	runPS := func(script string) string {
		cmd := exec.Command("powershell.exe", "-NoLogo", "-NonInteractive", "-Command", script)
		var buf, errBuf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			preview := script
			if len(preview) > 40 {
				preview = preview[:40]
			}
			log.Printf("ps: %s: %v: %s", preview, err, strings.TrimSpace(errBuf.String()))
			return ""
		}
		return strings.TrimSpace(buf.String())
	}

	cpuOut := runPS(`(Get-Counter '\Processor(_Total)\% Processor Time' -SampleInterval 1 -MaxSamples 2).CounterSamples[-1].CookedValue`)
	if v, err := strconv.ParseFloat(cpuOut, 64); err == nil {
		s.CPU = math.Round(v*10) / 10
	} else {
		s.LastError = "cpu counter failed"
	}

	memOut := runPS(`$o = Get-CimInstance Win32_OperatingSystem; "$($o.TotalVisibleMemorySize) $($o.FreePhysicalMemory)"`)
	if fields := strings.Fields(memOut); len(fields) == 2 {
		tot, _ := strconv.ParseUint(fields[0], 10, 64)
		free, _ := strconv.ParseUint(fields[1], 10, 64)
		s.MemoryTotal = tot * 1024
		s.MemoryUsed = (tot - free) * 1024
		if tot > 0 {
			s.MemoryPercent = float64(tot-free) / float64(tot) * 100
		}
	}

	diskOut := runPS(`$d = Get-PSDrive C; "$($d.Used) $($d.Free)"`)
	if fields := strings.Fields(diskOut); len(fields) == 2 {
		used, _ := strconv.ParseUint(fields[0], 10, 64)
		free, _ := strconv.ParseUint(fields[1], 10, 64)
		s.DiskUsed = used
		s.DiskTotal = used + free
		if s.DiskTotal > 0 {
			s.DiskPercent = float64(used) / float64(s.DiskTotal) * 100
		}
	}

	uptimeOut := runPS(`((Get-Date) - (Get-CimInstance Win32_OperatingSystem).LastBootUpTime).TotalSeconds`)
	if f, err := strconv.ParseFloat(uptimeOut, 64); err == nil {
		s.UptimeSeconds = int64(f)
	}

	dRunOut := runPS(`(docker ps -q 2>$null | Measure-Object -Line).Lines`)
	if r, err := strconv.Atoi(strings.TrimSpace(dRunOut)); err == nil {
		dTotOut := runPS(`(docker ps -aq 2>$null | Measure-Object -Line).Lines`)
		t, _ := strconv.Atoi(strings.TrimSpace(dTotOut))
		s.Docker = &DockerInfo{Running: r, Total: t}
	}

	return s
}

// ============================================================================
// MikroTik collector
// ============================================================================

func collectMikroTik(pool *sshPool, m MachineConfig) *MachineStatus {
	s := &MachineStatus{
		Name: m.Name, Host: m.Host, Port: m.Port,
		User: m.User, Type: m.Type, LastUpdate: time.Now(),
	}

	client, err := pool.connect(m)
	if err != nil {
		s.LastError = err.Error()
		return s
	}

	s.CPU, _ = strconv.ParseFloat(strings.TrimSpace(runSSH(client, ":put [/system/resource/get cpu-load]")), 64)

	freeMem, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get free-memory]")), 10, 64)
	totalMem, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get total-memory]")), 10, 64)
	s.MemoryTotal = totalMem
	s.MemoryUsed = totalMem - freeMem
	if totalMem > 0 {
		s.MemoryPercent = float64(totalMem-freeMem) / float64(totalMem) * 100
	}

	freeHDD, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get free-hdd-space]")), 10, 64)
	totalHDD, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get total-hdd-space]")), 10, 64)
	s.DiskTotal = totalHDD
	s.DiskUsed = totalHDD - freeHDD
	if totalHDD > 0 {
		s.DiskPercent = float64(totalHDD-freeHDD) / float64(totalHDD) * 100
	}

	s.UptimeSeconds = parseMikroTikUptime(strings.TrimSpace(runSSH(client, ":put [/system/resource/get uptime]")))

	var wifiCount int
	wcOut := runSSH(client, "/caps-man registration-table print count-only")
	if n, err := strconv.Atoi(strings.TrimSpace(wcOut)); err == nil {
		wifiCount = n
	} else {
		wcOut2 := runSSH(client, "/interface wireless registration-table print count-only")
		wifiCount, _ = strconv.Atoi(strings.TrimSpace(wcOut2))
	}

	ethRx, _ := strconv.ParseUint(strings.TrimSpace(
		runSSH(client, `:put [/interface ethernet get [find name="ether1"] rx-byte]`)), 10, 64)
	ethTx, _ := strconv.ParseUint(strings.TrimSpace(
		runSSH(client, `:put [/interface ethernet get [find name="ether1"] tx-byte]`)), 10, 64)

	s.MikroTik = &MikroTikInfo{WiFiClients: wifiCount, ETHRx: ethRx, ETHTx: ethTx}
	return s
}

func parseMikroTikUptime(s string) int64 {
	var total int64
	cur := ""
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			cur += string(ch)
		} else {
			v, _ := strconv.ParseInt(cur, 10, 64)
			cur = ""
			switch ch {
			case 'w':
				total += v * 604800
			case 'd':
				total += v * 86400
			case 'h':
				total += v * 3600
			case 'm':
				total += v * 60
			case 's':
				total += v
			}
		}
	}
	return total
}

// ============================================================================
// State store
// ============================================================================

type store struct {
	mu     sync.RWMutex
	states []*MachineStatus
}

func (s *store) update(idx int, ms *MachineStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[idx] = ms
}

func (s *store) snapshot() []*MachineStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MachineStatus, len(s.states))
	copy(out, s.states)
	return out
}

// ============================================================================
// WebSocket hub (RFC 6455, text frames only)
// ============================================================================

type wsConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *wsConn) send(msg []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return writeTextFrame(w.conn, msg)
}

func writeTextFrame(conn net.Conn, payload []byte) error {
	n := len(payload)
	var header []byte
	header = append(header, 0x81) // FIN + text opcode
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 65536:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127,
			0, 0, 0, 0,
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

type hub struct {
	mu    sync.Mutex
	conns map[*wsConn]struct{}
}

func newHub() *hub { return &hub{conns: make(map[*wsConn]struct{})} }

func (h *hub) add(c *wsConn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(c *wsConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	list := make([]*wsConn, 0, len(h.conns))
	for c := range h.conns {
		list = append(list, c)
	}
	h.mu.Unlock()

	for _, c := range list {
		if err := c.send(msg); err != nil {
			c.conn.Close()
			h.remove(c)
		}
	}
}

// wsHandshake upgrades an HTTP connection to WebSocket.
func wsHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	// RFC 6455 accept hash
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New() //nolint:gosec -- required by RFC 6455
	io.WriteString(h, key+magic) //nolint:errcheck
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("server does not support hijacking")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	buf.WriteString(response) //nolint:errcheck
	buf.Flush()               //nolint:errcheck

	return conn, nil
}

// drainWS reads and discards incoming WebSocket frames (ping / close).
func drainWS(conn net.Conn) {
	buf := make([]byte, 256)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}

// ============================================================================
// Poll loop
// ============================================================================

func pollOnce(cfg *Config, pool *sshPool, st *store, t *Thresholds) {
	var wg sync.WaitGroup
	for i, m := range cfg.Machines {
		wg.Add(1)
		go func(idx int, mc MachineConfig) {
			defer wg.Done()
			var s *MachineStatus
			switch mc.Type {
			case TypeLinux:
				s = collectLinux(pool, mc)
			case TypeWindows:
				s = collectWindows(mc)
			case TypeMikroTik:
				s = collectMikroTik(pool, mc)
			default:
				s = &MachineStatus{Name: mc.Name, LastError: "unknown machine type", LastUpdate: time.Now()}
			}
			s.Status = scoreHealth(s, t)
			st.update(idx, s)
		}(i, m)
	}
	wg.Wait()
}

// ============================================================================
// HTTP handlers
// ============================================================================

func buildPush(states []*MachineStatus) ([]byte, error) {
	type pushMsg struct {
		Type string           `json:"type"`
		Data []*MachineStatus `json:"data"`
	}
	return json.Marshal(pushMsg{Type: "machines", Data: states})
}

func serveHTTP(cfg *Config, st *store, h *hub) {
	mux := http.NewServeMux()

	// Serve embedded SPA
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML) //nolint:errcheck
	})

	// REST snapshot
	mux.HandleFunc("/api/machines", func(w http.ResponseWriter, r *http.Request) {
		states := st.snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(states) //nolint:errcheck
	})

	// WebSocket endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsHandshake(w, r)
		if err != nil {
			http.Error(w, "WebSocket upgrade failed", http.StatusBadRequest)
			return
		}

		c := &wsConn{conn: conn}
		h.add(c)
		defer func() {
			h.remove(c)
			conn.Close()
		}()

		// Send current state immediately on connect
		if msg, err := buildPush(st.snapshot()); err == nil {
			c.send(msg) //nolint:errcheck
		}

		// Drain incoming frames (keep-alive / close)
		drainWS(conn)
	})

	addr := "localhost:9800"
	log.Printf("Dashboard listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("HTTP server: %v", err)
	}
}

// ============================================================================
// Main
// ============================================================================

// portInUse checks if a TCP port is already bound.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func main() {
	cfgPath := "machines.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	// Idempotent: if port already in use, another instance is serving
	if portInUse(9800) {
		log.Printf("Port 9800 already in use — another instance is running. Exiting.")
		// Block forever so Wave's cmd:persistent keeps the block alive
		select {}
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	pool := newSSHPool(cfg.SSHTimeoutSeconds)
	defer pool.close()

	st := &store{
		states: make([]*MachineStatus, len(cfg.Machines)),
	}
	for i, m := range cfg.Machines {
		st.states[i] = &MachineStatus{
			Name: m.Name, Host: m.Host, Port: m.Port,
			User: m.User, Type: m.Type,
			Status: HealthUnknown, LastUpdate: time.Now(),
		}
	}

	h := newHub()
	t := defaultThresholds

	// HTTP server in background
	go serveHTTP(cfg, st, h)

	// Initial poll
	log.Printf("Running initial poll for %d machines...", len(cfg.Machines))
	pollOnce(cfg, pool, st, t)
	log.Printf("Initial poll complete.")

	// Broadcast loop
	pushFn := func() {
		if msg, err := buildPush(st.snapshot()); err == nil {
			h.broadcast(msg)
		}
	}
	pushFn()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pollOnce(cfg, pool, st, t)
			pushFn()
		case <-ctx.Done():
			log.Println("Shutting down.")
			return
		}
	}
}
