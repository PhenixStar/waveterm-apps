package main

import (
	"bytes"
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
// SSH pool
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

func (p *sshPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.Close()
	}
	p.clients = make(map[string]*gossh.Client)
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
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Run() //nolint:errcheck
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
// Poll
// ============================================================================

func pollAll(cfg *Config, pool *sshPool) []*MachineStatus {
	results := make([]*MachineStatus, len(cfg.Machines))
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
				s = &MachineStatus{Name: mc.Name, LastError: "unknown type", LastUpdate: time.Now()}
			}
			s.Status = scoreHealth(s, defaultThresholds)
			results[idx] = s
		}(i, m)
	}
	wg.Wait()
	return results
}
