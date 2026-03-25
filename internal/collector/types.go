// Package collector defines shared data types for machine metric collection.
// These types are extracted from cmd/dashboard/main.go for future refactoring.
//
// TODO (Phase 2): Move collectLinux, collectWindows, collectMikroTik here,
// then import from cmd/dashboard/ and cmd/docker-panel/.
package collector

import "time"

// MachineType identifies the OS/platform of a remote machine.
type MachineType string

const (
	TypeLinux    MachineType = "linux"
	TypeWindows  MachineType = "windows"
	TypeMikroTik MachineType = "mikrotik"
)

// HealthStatus is a severity level derived from metric thresholds.
type HealthStatus int

const (
	HealthOK HealthStatus = iota
	HealthWarning
	HealthCritical
	HealthUnknown
)

// MachineConfig holds connection parameters for one host.
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

// GPUInfo holds per-GPU metrics from nvidia-smi.
type GPUInfo struct {
	Index       int
	Name        string
	UtilPercent float64
	MemUsed     uint64
	MemTotal    uint64
	MemPercent  float64
	TempC       float64
}

// DockerInfo holds container counts from docker ps.
type DockerInfo struct {
	Running int
	Total   int
}

// MikroTikInfo holds RouterOS-specific metrics.
type MikroTikInfo struct {
	WiFiClients int
	ETHTx       uint64
	ETHRx       uint64
}

// MachineStatus is the complete health snapshot for one machine.
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
