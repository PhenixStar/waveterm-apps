// wt-docker-panel: Docker orchestration panel backend for Wave Terminal
//
// Serves HTTP + SSE API on localhost:9173.
// SSHes into each configured Docker host to run docker commands.
// Polls all hosts every 10s. Streams logs via SSE.
//
// Build:  go build -o bin/wt-docker-panel.exe ./cmd/docker-panel/
// Run:    bin/wt-docker-panel.exe [-port 9173] [-config hosts.json]
//
// Frontend: open frontend/docker-panel/docker-panel.tsx (served via Wave view:web)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ─────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────

type HostConfig struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	KeyFile string `json:"key_file"`
	Label   string `json:"label"`
}

type AppConfig struct {
	Hosts []HostConfig `json:"hosts"`
}

func loadConfig(path string) (*AppConfig, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "waveterm", "waveapps", "docker-panel", "hosts.json")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		// Return default config pointing at DGX1
		return defaultConfig(), nil
	}

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, nil
}

func defaultConfig() *AppConfig {
	home, _ := os.UserHomeDir()
	return &AppConfig{
		Hosts: []HostConfig{
			{
				Name:    "dgx1",
				Host:    "10.10.101.13",
				Port:    2222,
				User:    "phenix",
				KeyFile: filepath.Join(home, ".ssh", "id_rsa"),
				Label:   "DGX1 V100",
			},
		},
	}
}

// ─────────────────────────────────────────────
// Data types
// ─────────────────────────────────────────────

// Container maps docker ps --format '{{json .}}' output
type Container struct {
	ID         string `json:"ID"`
	Names      string `json:"Names"`
	Image      string `json:"Image"`
	Status     string `json:"Status"`
	State      string `json:"State"`
	Ports      string `json:"Ports"`
	CreatedAt  string `json:"CreatedAt"`
	RunningFor string `json:"RunningFor"`
	// Injected by backend
	Host string `json:"host"`
}

// ContainerStats maps docker stats --no-stream --format '{{json .}}' output
type ContainerStats struct {
	Name        string `json:"Name"`
	CPUPerc     string `json:"CPUPerc"`
	MemUsage    string `json:"MemUsage"`
	MemPerc     string `json:"MemPerc"`
	NetIO       string `json:"NetIO"`
	BlockIO     string `json:"BlockIO"`
	PIDs        string `json:"PIDs"`
	ContainerID string `json:"Container"`
	// Injected by backend
	Host string `json:"host"`
}

type HostStatus struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Reachable   bool        `json:"reachable"`
	LastChecked time.Time   `json:"last_checked"`
	Error       string      `json:"error,omitempty"`
	Containers  []Container `json:"containers"`
}

// ─────────────────────────────────────────────
// SSH helpers
// ─────────────────────────────────────────────

func sshClientConfig(h HostConfig) (*ssh.ClientConfig, error) {
	keyPath := h.KeyFile
	if strings.HasPrefix(keyPath, "~") {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, keyPath[1:])
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
		User:            h.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: use known_hosts in production
		Timeout:         10 * time.Second,
	}, nil
}

func sshDial(h HostConfig) (*ssh.Client, error) {
	cfg, err := sshClientConfig(h)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", h.Host, h.Port)
	return ssh.Dial("tcp", addr, cfg)
}

func runSSH(h HostConfig, cmd string) (string, error) {
	client, err := sshDial(h)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", h.Name, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// ─────────────────────────────────────────────
// Docker operations
// ─────────────────────────────────────────────

func ListContainers(h HostConfig) ([]Container, error) {
	out, err := runSSH(h, `docker ps -a --format '{{json .}}'`)
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", h.Name, err)
	}

	var containers []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c Container
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			log.Printf("warn: parse container line on %s: %v", h.Name, err)
			continue
		}
		c.Host = h.Name
		containers = append(containers, c)
	}
	return containers, nil
}

func GetStats(h HostConfig) ([]ContainerStats, error) {
	out, err := runSSH(h, `docker stats --no-stream --format '{{json .}}'`)
	if err != nil {
		return nil, fmt.Errorf("stats on %s: %w", h.Name, err)
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
		s.Host = h.Name
		stats = append(stats, s)
	}
	return stats, nil
}

func StartContainer(h HostConfig, id string) error {
	_, err := runSSH(h, fmt.Sprintf("docker start %s", shellEscape(id)))
	return err
}

func StopContainer(h HostConfig, id string) error {
	_, err := runSSH(h, fmt.Sprintf("docker stop %s", shellEscape(id)))
	return err
}

func RestartContainer(h HostConfig, id string) error {
	_, err := runSSH(h, fmt.Sprintf("docker restart %s", shellEscape(id)))
	return err
}

func RemoveContainer(h HostConfig, id string) error {
	_, err := runSSH(h, fmt.Sprintf("docker rm -f %s", shellEscape(id)))
	return err
}

func GetLogs(h HostConfig, id string, tail int) (string, error) {
	cmd := fmt.Sprintf("docker logs --tail %d %s 2>&1", tail, shellEscape(id))
	return runSSH(h, cmd)
}

func InspectContainer(h HostConfig, id string) (string, error) {
	return runSSH(h, fmt.Sprintf("docker inspect %s", shellEscape(id)))
}

// StreamLogs opens an SSH session running docker logs -f and writes each line to the writer.
// Blocks until ctx is cancelled or SSH session ends.
func StreamLogs(ctx context.Context, h HostConfig, id string, tail int, w io.Writer) error {
	client, err := sshDial(h)
	if err != nil {
		return err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return err
	}

	cmd := fmt.Sprintf("docker logs -f --tail %d %s 2>&1", tail, shellEscape(id))
	if err := sess.Start(cmd); err != nil {
		return err
	}

	// Merge stdout + stderr into a single reader
	combined := io.MultiReader(stdout, stderr)
	scanner := bufio.NewScanner(combined)

	// Watch for context cancellation
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()

	for scanner.Scan() {
		line := scanner.Text()
		// SSE format: "data: {line}\n\n"
		_, err := fmt.Fprintf(w, "data: %s\n\n", line)
		if err != nil {
			break
		}
		// Flush if the writer supports it
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	close(done)
	return scanner.Err()
}

// shellEscape is a minimal single-argument escape — only handles container IDs/names
// which are alphanumeric + underscore + dash.
func shellEscape(s string) string {
	// Whitelist: container IDs are hex or alphanumeric names
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return "''" // reject unexpected chars
		}
	}
	return s
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
	return &State{
		hostStatus: make(map[string]bool),
	}
}

func (s *State) poll(hosts []HostConfig) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	allContainers := make([]Container, 0)
	allStats := make([]ContainerStats, 0)
	hostOK := make(map[string]bool)

	for _, h := range hosts {
		h := h
		wg.Add(1)
		go func() {
			defer wg.Done()

			containers, err := ListContainers(h)
			if err != nil {
				log.Printf("poll %s: %v", h.Name, err)
				mu.Lock()
				hostOK[h.Name] = false
				mu.Unlock()
				return
			}

			stats, err := GetStats(h)
			if err != nil {
				log.Printf("stats %s: %v", h.Name, err)
			}

			mu.Lock()
			allContainers = append(allContainers, containers...)
			allStats = append(allStats, stats...)
			hostOK[h.Name] = true
			mu.Unlock()
		}()
	}

	wg.Wait()

	s.mu.Lock()
	s.containers = allContainers
	s.stats = allStats
	s.hostStatus = hostOK
	s.mu.Unlock()
}

// ─────────────────────────────────────────────
// HTTP handlers
// ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

type Server struct {
	cfg   *AppConfig
	state *State
}

func (srv *Server) hostByName(name string) (HostConfig, bool) {
	for _, h := range srv.cfg.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return HostConfig{}, false
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

func (srv *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	srv.state.mu.RLock()
	defer srv.state.mu.RUnlock()

	type hostInfo struct {
		Name      string `json:"name"`
		Label     string `json:"label"`
		Reachable bool   `json:"reachable"`
	}

	result := make([]hostInfo, 0, len(srv.cfg.Hosts))
	for _, h := range srv.cfg.Hosts {
		result = append(result, hostInfo{
			Name:      h.Name,
			Label:     h.Label,
			Reachable: srv.state.hostStatus[h.Name],
		})
	}
	writeJSON(w, result)
}

// handleContainerAction handles POST /api/containers/{host}/{id}/start|stop|restart
// and DELETE /api/containers/{host}/{id}
func (srv *Server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	// Path: /api/containers/{host}/{id}/{action}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: [api, containers, host, id, action?]
	if len(parts) < 4 {
		writeError(w, 400, "invalid path")
		return
	}

	hostName := parts[2]
	containerID := parts[3]
	action := ""
	if len(parts) >= 5 {
		action = parts[4]
	}

	h, ok := srv.hostByName(hostName)
	if !ok {
		writeError(w, 404, "host not found: "+hostName)
		return
	}

	var err error
	switch {
	case r.Method == http.MethodDelete:
		err = RemoveContainer(h, containerID)
	case action == "start":
		err = StartContainer(h, containerID)
	case action == "stop":
		err = StopContainer(h, containerID)
	case action == "restart":
		err = RestartContainer(h, containerID)
	default:
		writeError(w, 405, "unknown action: "+action)
		return
	}

	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Re-poll this host immediately to get fresh state
	go srv.state.poll([]HostConfig{h})

	writeJSON(w, map[string]string{"status": "ok"})
}

func (srv *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /api/containers/{host}/{id}/logs[/stream]
	if len(parts) < 5 {
		writeError(w, 400, "invalid path")
		return
	}

	hostName := parts[2]
	containerID := parts[3]
	stream := len(parts) >= 6 && parts[5] == "stream"

	h, ok := srv.hostByName(hostName)
	if !ok {
		writeError(w, 404, "host not found: "+hostName)
		return
	}

	tail := 200
	if t := r.URL.Query().Get("tail"); t != "" {
		fmt.Sscanf(t, "%d", &tail)
	}

	if stream {
		// SSE streaming
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ctx := r.Context()
		if err := StreamLogs(ctx, h, containerID, tail, w); err != nil {
			log.Printf("stream logs %s/%s: %v", hostName, containerID, err)
		}
		return
	}

	// Static log fetch
	logs, err := GetLogs(h, containerID, tail)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"logs": logs})
}

func (srv *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 {
		writeError(w, 400, "invalid path")
		return
	}

	hostName := parts[2]
	containerID := parts[3]

	h, ok := srv.hostByName(hostName)
	if !ok {
		writeError(w, 404, "host not found")
		return
	}

	out, err := InspectContainer(h, containerID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, out)
}

// ─────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────

func main() {
	port := flag.Int("port", 9173, "HTTP server port")
	configPath := flag.String("config", "", "Path to hosts.json config")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("Loaded %d host(s)", len(cfg.Hosts))

	state := newState()
	srv := &Server{cfg: cfg, state: state}

	// Initial poll
	log.Println("Initial poll...")
	state.poll(cfg.Hosts)
	log.Printf("Found %d containers", len(state.containers))

	// Background polling every 10s
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			state.poll(cfg.Hosts)
		}
	}()

	// Routes
	mux := http.NewServeMux()

	// CORS preflight
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}

		path := r.URL.Path
		switch {
		case path == "/api/containers" && r.Method == http.MethodGet:
			srv.handleContainers(w, r)
		case path == "/api/stats" && r.Method == http.MethodGet:
			srv.handleStats(w, r)
		case path == "/api/hosts" && r.Method == http.MethodGet:
			srv.handleHosts(w, r)
		case strings.Contains(path, "/logs/stream"):
			srv.handleLogs(w, r)
		case strings.HasSuffix(path, "/logs"):
			srv.handleLogs(w, r)
		case strings.HasSuffix(path, "/inspect"):
			srv.handleInspect(w, r)
		case strings.Contains(path, "/containers/") &&
			(r.Method == http.MethodPost || r.Method == http.MethodDelete):
			srv.handleContainerAction(w, r)
		default:
			writeError(w, 404, "not found")
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Docker panel backend listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
