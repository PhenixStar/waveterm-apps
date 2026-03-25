// wt-network-topology: Network topology viewer for Wave Terminal
//
// TODO: Implement backend server that:
//   - Loads topology-data.json
//   - Serves it via HTTP GET /api/topology
//   - Optionally polls device status via SSH ping
//   - Exposes WebSocket /ws/status for live node status updates
//
// Build:  go build -o bin/wt-network-topology.exe ./cmd/network-topology/
// Frontend: frontend/network-topology/topology-component.tsx (React + D3)
//
// Status: scaffold — frontend is ready, backend is TODO
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wt-network-topology: not yet implemented")
	fmt.Fprintln(os.Stderr, "Frontend reference: frontend/network-topology/topology-component.tsx")
	fmt.Fprintln(os.Stderr, "Data: frontend/network-topology/topology-data.json")
	os.Exit(1)
}
