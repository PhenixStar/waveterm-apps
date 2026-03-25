// wt-docker-panel-web: Web-based Docker orchestration panel for Wave Terminal
//
// Serves an embedded HTML SPA + REST API + WebSocket on localhost:9801.
// SSHes into Linux Docker hosts (from machines.json) to run docker commands.
// Polls all hosts every 15s. Streams logs via SSE. Pushes live stats via WS.
//
// Build:  go build -o bin/wt-docker-panel-web.exe ./cmd/docker-panel-web/
// Run:    bin/wt-docker-panel-web.exe [-port 9801] [-config machines.json]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

// ─────────────────────────────────────────────
// Config — reads machines.json
// ─────────────────────────────────────────────

type Machine struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Type    string `json:"type"`
	KeyPath string `json:"keyPath"`
	Comment string `json:"comment"`
}

type MachinesConfig struct {
	PollIntervalSeconds int       `json:"pollIntervalSeconds"`
	SSHTimeoutSeconds   int       `json:"sshTimeoutSeconds"`
	Machines            []Machine `json:"machines"`
}

func loadMachines(path string) ([]Machine, error) {
	if path == "" {
		// default: look next to the binary, then cwd
		exe, _ := os.Executable()
		candidates := []string{
			filepath.Join(filepath.Dir(exe), "machines.json"),
			"machines.json",
			filepath.Join(os.Getenv("USERPROFILE"), ".config", "waveterm", "machines.json"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}

	if path == "" {
		log.Println("warn: no machines.json found, using built-in DGX1 default")
		return defaultMachines(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg MachinesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		// try bare array
		var machines []Machine
		if err2 := json.Unmarshal(data, &machines); err2 != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return machines, nil
	}

	// Filter: only linux machines with a port (Docker hosts)
	var dockerHosts []Machine
	for _, m := range cfg.Machines {
		if m.Type == "linux" && m.Port > 0 {
			dockerHosts = append(dockerHosts, m)
		}
	}
	if len(dockerHosts) == 0 {
		log.Println("warn: no linux machines found in config, using built-in default")
		return defaultMachines(), nil
	}
	return dockerHosts, nil
}

func defaultMachines() []Machine {
	home, _ := os.UserHomeDir()
	return []Machine{
		{
			Name:    "DGX1-V100",
			Host:    "120.28.138.55",
			Port:    2442,
			User:    "phenix",
			Type:    "linux",
			KeyPath: filepath.Join(home, ".ssh", "id_ed25519"),
		},
	}
}

// ─────────────────────────────────────────────
// Data types
// ─────────────────────────────────────────────

type Container struct {
	ID         string `json:"ID"`
	Names      string `json:"Names"`
	Image      string `json:"Image"`
	Status     string `json:"Status"`
	State      string `json:"State"`
	Ports      string `json:"Ports"`
	CreatedAt  string `json:"CreatedAt"`
	RunningFor string `json:"RunningFor"`
	Host       string `json:"host"`
}

type ContainerStats struct {
	Name        string `json:"Name"`
	CPUPerc     string `json:"CPUPerc"`
	MemUsage    string `json:"MemUsage"`
	MemPerc     string `json:"MemPerc"`
	NetIO       string `json:"NetIO"`
	BlockIO     string `json:"BlockIO"`
	PIDs        string `json:"PIDs"`
	ContainerID string `json:"Container"`
	Host        string `json:"host"`
}

// ─────────────────────────────────────────────
// SSH helpers
// ─────────────────────────────────────────────

func sshConfig(m Machine) (*ssh.ClientConfig, error) {
	keyPath := m.KeyPath
	if strings.HasPrefix(keyPath, "~") {
		home, _ := os.UserHomeDir()
		keyPath = home + keyPath[1:]
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", keyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}

	return &ssh.ClientConfig{
		User:            m.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

func sshDial(m Machine) (*ssh.Client, error) {
	cfg, err := sshConfig(m)
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", fmt.Sprintf("%s:%d", m.Host, m.Port), cfg)
}

func runSSH(m Machine, cmd string) (string, error) {
	client, err := sshDial(m)
	if err != nil {
		return "", err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// shellSafe validates container IDs/names (alphanum, dash, underscore, dot only)
func shellSafe(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return len(s) > 0
}

// ─────────────────────────────────────────────
// Docker operations
// ─────────────────────────────────────────────

func listContainers(m Machine) ([]Container, error) {
	out, err := runSSH(m, `docker ps -a --format '{{json .}}'`)
	if err != nil {
		return nil, err
	}

	var containers []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c Container
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			log.Printf("warn parse container on %s: %v", m.Name, err)
			continue
		}
		c.Host = m.Name
		containers = append(containers, c)
	}
	return containers, nil
}

func getStats(m Machine) ([]ContainerStats, error) {
	out, err := runSSH(m, `docker stats --no-stream --format '{{json .}}'`)
	if err != nil {
		return nil, err
	}

	var stats []ContainerStats
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s ContainerStats
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		s.Host = m.Name
		stats = append(stats, s)
	}
	return stats, nil
}

func containerAction(m Machine, id, action string) error {
	if !shellSafe(id) {
		return fmt.Errorf("invalid container id: %q", id)
	}
	cmds := map[string]string{
		"start":   "docker start " + id,
		"stop":    "docker stop " + id,
		"restart": "docker restart " + id,
		"remove":  "docker rm -f " + id,
	}
	cmd, ok := cmds[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}
	_, err := runSSH(m, cmd)
	return err
}

func getLogs(m Machine, id string, tail int) (string, error) {
	if !shellSafe(id) {
		return "", fmt.Errorf("invalid container id")
	}
	return runSSH(m, fmt.Sprintf("docker logs --tail %d %s 2>&1", tail, id))
}

func streamLogs(ctx context.Context, m Machine, id string, tail int, w io.Writer) error {
	if !shellSafe(id) {
		return fmt.Errorf("invalid container id")
	}

	client, err := sshDial(m)
	if err != nil {
		return err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	stdout, _ := sess.StdoutPipe()
	stderr, _ := sess.StderrPipe()
	combined := io.MultiReader(stdout, stderr)

	if err := sess.Start(fmt.Sprintf("docker logs -f --tail %d %s 2>&1", tail, id)); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()

	scanner := bufio.NewScanner(combined)
	for scanner.Scan() {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", scanner.Text()); err != nil {
			break
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	close(done)
	return scanner.Err()
}

// ─────────────────────────────────────────────
// State / polling
// ─────────────────────────────────────────────

type State struct {
	mu         sync.RWMutex
	containers []Container
	stats      []ContainerStats
	hostStatus map[string]bool
}

func newState() *State {
	return &State{hostStatus: make(map[string]bool)}
}

func (s *State) poll(machines []Machine) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allC []Container
	var allS []ContainerStats
	hostOK := make(map[string]bool)

	for _, m := range machines {
		m := m
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers, err := listContainers(m)
			if err != nil {
				log.Printf("poll %s: %v", m.Name, err)
				mu.Lock()
				hostOK[m.Name] = false
				mu.Unlock()
				return
			}
			stats, _ := getStats(m)
			mu.Lock()
			allC = append(allC, containers...)
			allS = append(allS, stats...)
			hostOK[m.Name] = true
			mu.Unlock()
		}()
	}

	wg.Wait()

	s.mu.Lock()
	s.containers = allC
	s.stats = allS
	s.hostStatus = hostOK
	s.mu.Unlock()
}

// ─────────────────────────────────────────────
// WebSocket hub
// ─────────────────────────────────────────────

type WSHub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

func newWSHub() *WSHub {
	return &WSHub{clients: make(map[*websocket.Conn]bool)}
}

func (h *WSHub) add(c *websocket.Conn) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *WSHub) remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *WSHub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		websocket.Message.Send(c, string(msg)) //nolint:errcheck
	}
}

// ─────────────────────────────────────────────
// HTTP Server
// ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

type Server struct {
	machines []Machine
	state    *State
	hub      *WSHub
}

func (srv *Server) machineByName(name string) (Machine, bool) {
	for _, m := range srv.machines {
		if m.Name == name {
			return m, true
		}
	}
	return Machine{}, false
}

func (srv *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func (srv *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	type hostInfo struct {
		Name      string `json:"name"`
		Label     string `json:"label"`
		Reachable bool   `json:"reachable"`
	}

	srv.state.mu.RLock()
	defer srv.state.mu.RUnlock()

	result := make([]hostInfo, 0, len(srv.machines))
	for _, m := range srv.machines {
		result = append(result, hostInfo{
			Name:      m.Name,
			Label:     m.Name,
			Reachable: srv.state.hostStatus[m.Name],
		})
	}
	writeJSON(w, result)
}

func (srv *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	srv.state.mu.RLock()
	defer srv.state.mu.RUnlock()
	writeJSON(w, srv.state.containers)
}

func (srv *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	srv.state.mu.RLock()
	defer srv.state.mu.RUnlock()
	writeJSON(w, srv.state.stats)
}

// handleAPI dispatches all /api/* routes
func (srv *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}

	path := r.URL.Path
	// strip leading /
	path = strings.TrimPrefix(path, "/")
	// segments: api, containers, [host, id, action?]
	parts := strings.Split(path, "/")

	switch {
	case path == "api/containers" && r.Method == http.MethodGet:
		srv.handleContainers(w, r)

	case path == "api/stats" && r.Method == http.MethodGet:
		srv.handleStats(w, r)

	case path == "api/hosts" && r.Method == http.MethodGet:
		srv.handleHosts(w, r)

	case len(parts) >= 4 && parts[0] == "api" && parts[1] == "containers":
		hostName := parts[2]
		containerID := parts[3]
		m, ok := srv.machineByName(hostName)
		if !ok {
			writeError(w, 404, "host not found: "+hostName)
			return
		}

		// /api/containers/{host}/{id}/logs[/stream]
		if len(parts) >= 5 && parts[4] == "logs" {
			stream := len(parts) >= 6 && parts[5] == "stream"
			tail := 200
			if t := r.URL.Query().Get("tail"); t != "" {
				fmt.Sscanf(t, "%d", &tail)
			}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				if err := streamLogs(r.Context(), m, containerID, tail, w); err != nil {
					log.Printf("stream logs %s/%s: %v", hostName, containerID, err)
				}
				return
			}
			logs, err := getLogs(m, containerID, tail)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			writeJSON(w, map[string]string{"logs": logs})
			return
		}

		// /api/containers/{host}/{id}/{action} or DELETE
		action := ""
		if len(parts) >= 5 {
			action = parts[4]
		}

		switch {
		case r.Method == http.MethodDelete:
			action = "remove"
		case r.Method == http.MethodPost && (action == "start" || action == "stop" || action == "restart"):
			// valid
		default:
			writeError(w, 405, "unknown action")
			return
		}

		if err := containerAction(m, containerID, action); err != nil {
			writeError(w, 500, err.Error())
			return
		}

		// Repoll this host immediately
		go srv.state.poll([]Machine{m})
		writeJSON(w, map[string]string{"status": "ok"})

	default:
		writeError(w, 404, "not found: "+r.URL.Path)
	}
}

// handleWS upgrades to WebSocket and holds the connection; client receives stat pushes
func (srv *Server) handleWS(ws *websocket.Conn) {
	srv.hub.add(ws)
	defer srv.hub.remove(ws)

	// Keep alive: read any message (ping), ignore errors
	buf := make([]byte, 256)
	for {
		if _, err := ws.Read(buf); err != nil {
			break
		}
	}
}

// statsBroadcastLoop pushes live stats via WS every 5 seconds
func (srv *Server) statsBroadcastLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		srv.state.mu.RLock()
		stats := srv.state.stats
		srv.state.mu.RUnlock()

		msg, _ := json.Marshal(map[string]interface{}{
			"type": "stats",
			"data": stats,
		})
		srv.hub.broadcast(msg)
	}
}

// ─────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────

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
	port := flag.Int("port", 9801, "HTTP listen port")
	configPath := flag.String("config", "", "Path to machines.json")
	flag.Parse()

	// Idempotent: if port already in use, another instance is serving
	if portInUse(*port) {
		log.Printf("Port %d already in use — another instance is running. Exiting.", *port)
		select {}
	}

	machines, err := loadMachines(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("Docker panel: %d host(s) configured", len(machines))

	state := newState()
	hub := newWSHub()
	srv := &Server{machines: machines, state: state, hub: hub}

	// Initial poll
	log.Println("Initial poll...")
	state.poll(machines)
	log.Printf("Found %d container(s)", len(state.containers))

	// Background polling every 15s
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			state.poll(machines)
		}
	}()

	// Stats broadcast loop
	go srv.statsBroadcastLoop()

	// Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", srv.handleAPI)
	mux.Handle("/ws", websocket.Handler(srv.handleWS))
	mux.HandleFunc("/", srv.handleIndex)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
