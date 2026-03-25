# waveterm-apps

Custom Wave Terminal apps for multi-machine infrastructure monitoring.

Module: `github.com/PhenixStar/waveterm-apps`

---

## Apps

### 1. `wt-dashboard` — Machine Health Monitor (ready to test)

ANSI terminal dashboard that polls CPU, memory, disk, GPU, Docker, and MikroTik
metrics from all configured machines via SSH (or PowerShell locally on Windows).
Renders in a Wave Terminal `view:term` block with live countdown.

**Status:** Fully implemented. Build and run.

### 2. `wt-docker-panel` — Docker Orchestration Panel (scaffold)

HTTP + SSE backend that connects to Docker hosts via SSH. Serves a JSON API
consumed by the React frontend (`frontend/docker-panel/docker-panel.tsx`).
Supports container start/stop/restart/remove, log streaming, and inspect.

**Status:** Backend implemented. Frontend is standalone React (not compiled here).

### 3. `wt-network-topology` — Network Topology Viewer (scaffold)

Interactive D3 force-graph of the full network topology with VLAN clusters,
live status dots, and click-to-inspect. React + D3 frontend.

**Status:** Frontend reference complete. Go backend is TODO (Phase 2).

---

## Build

Requires Go 1.22+.

```bash
# Build dashboard binary
go build -o bin/wt-dashboard.exe ./cmd/dashboard/

# Build docker-panel backend
go build -o bin/wt-docker-panel.exe ./cmd/docker-panel/

# Build both
make all

# Clean
make clean
```

---

## Usage

### Dashboard

Run directly:
```bash
bin/wt-dashboard.exe
# or with custom config:
bin/wt-dashboard.exe path/to/machines.json
```

Default config file: `machines.json` (in the same directory as the binary, or cwd).

### Docker Panel Backend

```bash
bin/wt-docker-panel.exe
# or:
bin/wt-docker-panel.exe -port 9173 -config /path/to/hosts.json
```

Then open `frontend/docker-panel/docker-panel.tsx` in a Wave Terminal `view:web`
pointing at `http://localhost:9173`.

---

## Wave Terminal Widget Entries

Add these to your `widgets.json` (usually `~/.config/waveterm/widgets.json`):

```json
"machine-health": {
  "display:order": 50,
  "icon": "heart-pulse",
  "color": "#10b981",
  "label": "health",
  "description": "Multi-machine SSH health dashboard — polls every 15s",
  "blockdef": {
    "meta": {
      "view": "term",
      "controller": "cmd",
      "cmd": "D:/Dev/waveterm-apps/bin/wt-dashboard.exe",
      "cmd:shell": false,
      "cmd:runonstart": true,
      "cmd:persistent": true,
      "cmd:cwd": "D:/Dev/waveterm-apps"
    }
  }
},
"docker-panel": {
  "display:order": 51,
  "icon": "container",
  "color": "#06b6d4",
  "label": "docker",
  "description": "Docker orchestration panel — multi-host container management",
  "blockdef": {
    "meta": {
      "view": "web",
      "url": "http://localhost:9173"
    }
  }
}
```

---

## Machine Config Format (`machines.json`)

```json
{
  "pollIntervalSeconds": 15,
  "sshTimeoutSeconds": 10,
  "machines": [
    {
      "name": "MyServer",
      "host": "192.168.1.10",
      "port": 22,
      "user": "admin",
      "type": "linux",
      "keyPath": "~/.ssh/id_ed25519"
    },
    {
      "name": "LocalWin",
      "host": "localhost",
      "port": 0,
      "user": "Kratos",
      "type": "windows",
      "keyPath": ""
    },
    {
      "name": "RouterOS",
      "host": "192.168.88.1",
      "port": 22,
      "user": "admin",
      "type": "mikrotik",
      "keyPath": "~/.ssh/id_ed25519"
    }
  ]
}
```

**Machine types:**
- `linux` — SSH into Linux host, reads `/proc/*`, `nvidia-smi`, `docker ps`
- `windows` — Local PowerShell only (no SSH), reads Win32 WMI counters
- `mikrotik` — SSH into RouterOS, runs `:put [/system/resource/get ...]`

---

## Architecture

```
waveterm-apps/
├── cmd/dashboard/main.go       # Monolithic dashboard (poll + render loop)
├── cmd/docker-panel/main.go    # HTTP/SSE backend for React frontend
├── cmd/network-topology/main.go # Placeholder — Phase 2
├── internal/
│   ├── collector/types.go      # Shared data types (Phase 2 refactor target)
│   ├── ssh/pool.go             # SSH pool placeholder
│   └── render/ansi.go          # ANSI renderer placeholder
├── frontend/
│   ├── dashboard/              # React web version (optional)
│   ├── docker-panel/           # React + ANSI log viewer
│   └── network-topology/       # React + D3 force graph
└── machines.json               # Machine inventory
```

**Dashboard polling loop:**
```
startup → render skeleton → poll all machines (concurrent goroutines)
ticker (15s) → re-poll → re-render
countTicker (1s) → re-render countdown footer only
```

**SSH strategy:**
- One `*gossh.Client` per `user@host:port`, reused across polls (keepalive test)
- Linux CPU: two `/proc/stat` reads 500ms apart (delta method)
- MikroTik: individual `:put` commands per metric (RouterOS limitation)
- Windows (Sweep): `powershell.exe` locally via `exec.Command` — no SSH

---

## Health Thresholds

| Metric | Warning | Critical |
|--------|---------|----------|
| CPU    | > 80%   | > 95%    |
| Memory | > 85%   | > 95%    |
| Disk   | > 80%   | > 90%    |
| GPU util | > 80% | > 95%   |
| GPU mem  | > 85% | > 95%   |

Any SSH connection failure = HealthUnknown (gray dot, "UNREACHABLE").

---

## Status

| App | Go backend | React frontend | Notes |
|-----|-----------|----------------|-------|
| wt-dashboard | Ready | Ready (optional) | ANSI term is primary UI |
| wt-docker-panel | Ready | Ready | Run backend, open via view:web |
| wt-network-topology | TODO | Ready | Phase 2 |

---

## Phase 2 Roadmap

- Refactor `cmd/dashboard/main.go` into `internal/collector/`, `internal/ssh/`, `internal/render/`
- Add HTTP API endpoint to dashboard for React frontend at port 9876
- Implement `wt-network-topology` Go backend with `/api/topology` + WebSocket `/ws/status`
- Add SSH ping-based live status updates to topology
- Package frontend with esbuild into single-file bundles served by Go
