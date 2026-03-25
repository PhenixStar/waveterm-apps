// wt-dashboard: Wave Terminal multi-machine health dashboard
//
// Build:  go build -o bin/wt-dashboard.exe ./cmd/dashboard/
// Run:    bin/wt-dashboard.exe [path/to/machines.json]
//
// Widget entry (widgets.json):
//   "blockdef": { "meta": { "view": "term", "controller": "cmd",
//     "cmd": "D:/Dev/waveterm-apps/bin/wt-dashboard.exe",
//     "cmd:persistent": true, "cmd:cwd": "D:/Dev/waveterm-apps" } }
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// ============================================================================
// Config
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
	Index       int
	Name        string
	UtilPercent float64
	MemUsed     uint64
	MemTotal    uint64
	MemPercent  float64
	TempC       float64
}

type DockerInfo struct {
	Running int
	Total   int
}

type MikroTikInfo struct {
	WiFiClients int
	ETHTx       uint64
	ETHRx       uint64
}

type MachineStatus struct {
	Name          string
	Host          string
	Port          int
	User          string
	Type          MachineType
	Status        HealthStatus
	LastUpdate    time.Time
	LastError     string
	CPU           float64
	MemoryPercent float64
	MemoryUsed    uint64
	MemoryTotal   uint64
	DiskPercent   float64
	DiskUsed      uint64
	DiskTotal     uint64
	UptimeSeconds int64
	GPUs          []GPUInfo
	Docker        *DockerInfo
	MikroTik      *MikroTikInfo
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

	p.mu.Lock()
	c := p.clients[key]
	p.mu.Unlock()

	// Test existing connection with keepalive
	if c != nil {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return c, nil
		}
		c.Close()
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
		User:            m.User,
		Auth:            auths,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec -- internal lab hosts
		Timeout:         p.timeout,
	}

	addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
	conn, err := gossh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	p.mu.Lock()
	p.clients[key] = conn
	p.mu.Unlock()
	return conn, nil
}

// runSSH opens a new session on an existing client and runs one command.
func runSSH(client *gossh.Client, cmd string) string {
	sess, err := client.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()
	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Run(cmd) // non-zero exit is intentional (docker unavailable, etc.)
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
// Linux collector (SSH)
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

	// CPU — /proc/stat delta (two reads 500ms apart)
	cpu, err := linuxCPU(client)
	if err != nil {
		s.LastError = fmt.Sprintf("cpu: %v", err)
	} else {
		s.CPU = cpu
	}

	// Memory — /proc/meminfo
	memUsed, memTotal := linuxMemory(client)
	s.MemoryUsed, s.MemoryTotal = memUsed, memTotal
	if memTotal > 0 {
		s.MemoryPercent = float64(memUsed) / float64(memTotal) * 100
	}

	// Disk — df -B1 /
	diskUsed, diskTotal := linuxDisk(client)
	s.DiskUsed, s.DiskTotal = diskUsed, diskTotal
	if diskTotal > 0 {
		s.DiskPercent = float64(diskUsed) / float64(diskTotal) * 100
	}

	// Uptime — /proc/uptime
	if fields := strings.Fields(runSSH(client, "cat /proc/uptime")); len(fields) > 0 {
		if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
			s.UptimeSeconds = int64(f)
		}
	}

	// GPU (optional)
	s.GPUs = linuxGPU(client)

	// Docker (optional)
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
		v *= 1024 // kB -> bytes
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
// Windows collector (localhost, PowerShell)
// ============================================================================

func collectWindows(m MachineConfig) *MachineStatus {
	s := &MachineStatus{
		Name: m.Name, Host: m.Host, Port: m.Port,
		User: m.User, Type: m.Type, LastUpdate: time.Now(),
	}

	// runPS executes a PowerShell expression and returns trimmed stdout.
	// Uses exec.Command with a fixed binary path — not shell injection risk.
	runPS := func(script string) string {
		args := []string{"-NoLogo", "-NonInteractive", "-Command", script}
		cmd := exec.Command("powershell.exe", args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Run() //nolint:errcheck -- errors reflected in empty output
		return strings.TrimSpace(buf.String())
	}

	// CPU — two counter samples, 1s apart
	cpuOut := runPS(`(Get-Counter '\Processor(_Total)\% Processor Time' -SampleInterval 1 -MaxSamples 2).CounterSamples[-1].CookedValue`)
	if v, err := strconv.ParseFloat(cpuOut, 64); err == nil {
		s.CPU = math.Round(v*10) / 10
	} else {
		s.LastError = "cpu counter failed"
	}

	// Memory — Win32_OperatingSystem (kB units)
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

	// Disk — C: drive via Get-PSDrive (bytes)
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

	// Uptime — seconds since last boot
	uptimeOut := runPS(`((Get-Date) - (Get-CimInstance Win32_OperatingSystem).LastBootUpTime).TotalSeconds`)
	if f, err := strconv.ParseFloat(uptimeOut, 64); err == nil {
		s.UptimeSeconds = int64(f)
	}

	// Docker (optional — no error if not installed)
	dRunOut := runPS(`(docker ps -q 2>$null | Measure-Object -Line).Lines`)
	if r, err := strconv.Atoi(strings.TrimSpace(dRunOut)); err == nil {
		dTotOut := runPS(`(docker ps -aq 2>$null | Measure-Object -Line).Lines`)
		t, _ := strconv.Atoi(strings.TrimSpace(dTotOut))
		s.Docker = &DockerInfo{Running: r, Total: t}
	}

	return s
}

// ============================================================================
// MikroTik collector (SSH, RouterOS)
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

	// CPU load (0-100)
	cpuOut := runSSH(client, ":put [/system/resource/get cpu-load]")
	s.CPU, _ = strconv.ParseFloat(strings.TrimSpace(cpuOut), 64)

	// Memory (bytes in RouterOS)
	freeMem, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get free-memory]")), 10, 64)
	totalMem, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get total-memory]")), 10, 64)
	s.MemoryTotal = totalMem
	s.MemoryUsed = totalMem - freeMem
	if totalMem > 0 {
		s.MemoryPercent = float64(totalMem-freeMem) / float64(totalMem) * 100
	}

	// Disk (HDD space in RouterOS)
	freeHDD, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get free-hdd-space]")), 10, 64)
	totalHDD, _ := strconv.ParseUint(strings.TrimSpace(runSSH(client, ":put [/system/resource/get total-hdd-space]")), 10, 64)
	s.DiskTotal = totalHDD
	s.DiskUsed = totalHDD - freeHDD
	if totalHDD > 0 {
		s.DiskPercent = float64(totalHDD-freeHDD) / float64(totalHDD) * 100
	}

	// Uptime (format: "5d12h30m15s")
	s.UptimeSeconds = parseMikroTikUptime(strings.TrimSpace(runSSH(client, ":put [/system/resource/get uptime]")))

	// WiFi clients — try CAPsMAN first, fall back to local wireless
	var wifiCount int
	wcOut := runSSH(client, "/caps-man registration-table print count-only")
	if n, err := strconv.Atoi(strings.TrimSpace(wcOut)); err == nil {
		wifiCount = n
	} else {
		wcOut2 := runSSH(client, "/interface wireless registration-table print count-only")
		wifiCount, _ = strconv.Atoi(strings.TrimSpace(wcOut2))
	}

	// ether1 cumulative bytes
	ethRx, _ := strconv.ParseUint(strings.TrimSpace(
		runSSH(client, `:put [/interface ethernet get [find name="ether1"] rx-byte]`)), 10, 64)
	ethTx, _ := strconv.ParseUint(strings.TrimSpace(
		runSSH(client, `:put [/interface ethernet get [find name="ether1"] tx-byte]`)), 10, 64)

	s.MikroTik = &MikroTikInfo{
		WiFiClients: wifiCount,
		ETHRx:       ethRx,
		ETHTx:       ethTx,
	}
	return s
}

// parseMikroTikUptime converts "5d12h30m15s" to seconds.
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
// ANSI terminal renderer
// ============================================================================

const (
	colReset  = "\033[0m"
	colRed    = "\033[91m"
	colYellow = "\033[93m"
	colGreen  = "\033[92m"
	colGray   = "\033[90m"
	colCyan   = "\033[96m"
	colBold   = "\033[1m"
	barWidth  = 8
)

func statusDot(h HealthStatus) string {
	switch h {
	case HealthOK:
		return colGreen + "\u25cf" + colReset
	case HealthWarning:
		return colYellow + "\u25cf" + colReset
	case HealthCritical:
		return colRed + "\u25cf" + colReset
	default:
		return colGray + "\u25cb" + colReset
	}
}

func renderBar(value float64, width int, t *Thresholds) string {
	v := math.Max(0, math.Min(100, value))
	filled := int(v / 100 * float64(width))
	color := colGreen
	switch {
	case v >= t.CPUCrit:
		color = colRed
	case v >= t.CPUWarn:
		color = colYellow
	}
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(color)
	b.WriteString(strings.Repeat("\u2588", filled))
	b.WriteString(colGray)
	b.WriteString(strings.Repeat("\u2591", width-filled))
	b.WriteString(colReset + "]")
	return b.String()
}

func fmtBytes(b uint64) string {
	units := []string{"B", "K", "M", "G", "T"}
	v := float64(b)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func fmtUptime(sec int64) string {
	if sec <= 0 {
		return "?"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func clearScreen() { fmt.Print("\033[2J\033[H") }

func renderDashboard(machines []*MachineStatus, t *Thresholds, nextSecs int) {
	clearScreen()
	now := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("%s%s  MACHINE HEALTH MONITOR  %s%s\n", colBold, colCyan, now, colReset)
	fmt.Printf("%s%s%s\n\n", colGray, strings.Repeat("-", 72), colReset)

	for _, m := range machines {
		dot := statusDot(m.Status)

		if m.Status == HealthUnknown && m.LastError != "" {
			ago := time.Since(m.LastUpdate).Round(time.Second)
			fmt.Printf("  %s %-14s %sUNREACHABLE%s  last %s ago\n",
				dot, m.Name, colRed, colReset, ago)
			fmt.Printf("               %s%s%s\n\n", colGray, truncate(m.LastError, 55), colReset)
			continue
		}

		cpuBar := renderBar(m.CPU, barWidth, t)
		memBar := renderBar(m.MemoryPercent, barWidth, t)
		diskBar := renderBar(m.DiskPercent, barWidth, t)

		fmt.Printf("  %s %-14s CPU%s%4.0f%%  MEM%s%4.0f%%  DISK%s%4.0f%%\n",
			dot, m.Name, cpuBar, m.CPU, memBar, m.MemoryPercent, diskBar, m.DiskPercent)

		fmt.Printf("               ")
		if len(m.GPUs) > 0 {
			var sumUtil, sumMem float64
			for _, g := range m.GPUs {
				sumUtil += g.UtilPercent
				sumMem += g.MemPercent
			}
			n := float64(len(m.GPUs))
			gpuBar := renderBar(sumUtil/n, 6, t)
			fmt.Printf("GPU%s%4.0f%%  GMEM%4.0f%%  x%d  ", gpuBar, sumUtil/n, sumMem/n, len(m.GPUs))
		}
		if m.Docker != nil {
			fmt.Printf("DOCKER %d/%d  ", m.Docker.Running, m.Docker.Total)
		}
		if m.MikroTik != nil {
			fmt.Printf("WiFi:%d cl  ETH rx:%s tx:%s  ",
				m.MikroTik.WiFiClients,
				fmtBytes(m.MikroTik.ETHRx),
				fmtBytes(m.MikroTik.ETHTx))
		}
		fmt.Printf("UP:%s\n\n", fmtUptime(m.UptimeSeconds))
	}

	fmt.Printf("%s%s%s\n", colGray, strings.Repeat("-", 72), colReset)
	fmt.Printf("%sNext: %ds%s  |  SSH: wsh ssh phenix@120.28.138.55:2442  |  Ctrl+C exit\n",
		colGray, nextSecs, colReset)
}

// ============================================================================
// Main
// ============================================================================

func main() {
	cfgPath := "machines.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	pool := newSSHPool(cfg.SSHTimeoutSeconds)
	defer pool.close()

	t := defaultThresholds
	states := make([]*MachineStatus, len(cfg.Machines))
	for i, m := range cfg.Machines {
		states[i] = &MachineStatus{
			Name: m.Name, Host: m.Host, Port: m.Port,
			User: m.User, Type: m.Type, Status: HealthUnknown,
		}
	}

	poll := func() {
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
					s = &MachineStatus{Name: mc.Name, LastError: "unknown machine type"}
				}
				s.Status = scoreHealth(s, t)
				states[idx] = s
			}(i, m)
		}
		wg.Wait()
	}

	// Initial skeleton render, then first real poll
	renderDashboard(states, t, cfg.PollIntervalSeconds)
	poll()
	renderDashboard(states, t, cfg.PollIntervalSeconds)

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	countTicker := time.NewTicker(time.Second)
	defer countTicker.Stop()

	countdown := cfg.PollIntervalSeconds
	for {
		select {
		case <-countTicker.C:
			countdown--
			if countdown <= 0 {
				countdown = cfg.PollIntervalSeconds
			}
			renderDashboard(states, t, countdown)
		case <-ticker.C:
			poll()
			countdown = cfg.PollIntervalSeconds
			renderDashboard(states, t, countdown)
		}
	}
}
