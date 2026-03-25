/**
 * MachineHealthDashboard — Wave Terminal multi-machine status grid
 *
 * Usage in a Wave Terminal WaveApp (vdom view, if/when it becomes available),
 * or as a standalone React page served via view:web.
 *
 * Props: pass `machines` directly OR leave empty and the component polls
 * GET /api/status every 15s automatically.
 */

import React, { useCallback, useEffect, useState } from "react";
import styles from "./dashboard-styles.module.css";

// ─── Types ───────────────────────────────────────────────────────────────────

export type HealthStatus = "ok" | "warning" | "critical" | "unknown";

export interface GPUInfo {
  index: number;
  name: string;
  utilPercent: number;
  memPercent: number;
  tempC: number;
  memUsedMB: number;
  memTotalMB: number;
}

export interface DockerInfo {
  running: number;
  total: number;
}

export interface MikroTikInfo {
  wifiClients: number;
  ethRxBytes: number;
  ethTxBytes: number;
}

export interface MachineStatus {
  name: string;
  host: string;
  port: number;
  status: HealthStatus;
  type: "linux" | "windows" | "mikrotik";
  cpu: number;        // 0-100
  memory: number;     // 0-100
  disk: number;       // 0-100
  uptime: number;     // seconds
  lastError?: string;
  lastSeen?: string;  // ISO timestamp
  gpus?: GPUInfo[];
  docker?: DockerInfo;
  mikrotik?: MikroTikInfo;
}

export interface DashboardProps {
  machines?: MachineStatus[];
  apiUrl?: string;        // default: /api/status
  pollIntervalMs?: number; // default: 15000
  onSSHClick?: (machine: MachineStatus) => void;
}

// ─── Utilities ───────────────────────────────────────────────────────────────

function fmtBytes(bytes: number): string {
  if (bytes === 0) return "0B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  return `${(bytes / Math.pow(1024, i)).toFixed(1)}${units[i]}`;
}

function fmtUptime(sec: number): string {
  if (!sec || sec <= 0) return "?";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function statusColor(status: HealthStatus): string {
  switch (status) {
    case "ok":       return "var(--color-ok, #22c55e)";
    case "warning":  return "var(--color-warning, #eab308)";
    case "critical": return "var(--color-critical, #ef4444)";
    default:         return "var(--color-unknown, #6b7280)";
  }
}

function metricColor(value: number, warnAt = 80, critAt = 95): string {
  if (value >= critAt)  return "var(--color-critical, #ef4444)";
  if (value >= warnAt)  return "var(--color-warning, #eab308)";
  return "var(--color-ok, #22c55e)";
}

function avgGPUUtil(gpus?: GPUInfo[]): number {
  if (!gpus || gpus.length === 0) return 0;
  return gpus.reduce((s, g) => s + g.utilPercent, 0) / gpus.length;
}

// ─── Sub-components ──────────────────────────────────────────────────────────

interface MetricBarProps {
  label: string;
  value: number;
  warnAt?: number;
  critAt?: number;
}

function MetricBar({ label, value, warnAt = 80, critAt = 95 }: MetricBarProps) {
  const color = metricColor(value, warnAt, critAt);
  return (
    <div className={styles.metric}>
      <span className={styles.metricLabel}>{label}</span>
      <div className={styles.barTrack}>
        <div
          className={styles.barFill}
          style={{
            width: `${Math.max(0, Math.min(100, value))}%`,
            backgroundColor: color,
            transition: "width 0.4s ease, background-color 0.3s ease",
          }}
        />
      </div>
      <span className={styles.metricValue} style={{ color }}>
        {value.toFixed(0)}%
      </span>
    </div>
  );
}

interface StatusDotProps {
  status: HealthStatus;
}

function StatusDot({ status }: StatusDotProps) {
  return (
    <span
      className={styles.statusDot}
      style={{ backgroundColor: statusColor(status) }}
      title={status}
    />
  );
}

// ─── Machine Tile ────────────────────────────────────────────────────────────

interface MachineTileProps {
  machine: MachineStatus;
  onSSHClick?: (m: MachineStatus) => void;
}

function MachineTile({ machine: m, onSSHClick }: MachineTileProps) {
  const [hovered, setHovered] = useState(false);
  const offline = m.status === "unknown" && !!m.lastError;
  const gpuAvg = avgGPUUtil(m.gpus);

  const handleClick = useCallback(() => {
    if (onSSHClick) onSSHClick(m);
  }, [m, onSSHClick]);

  return (
    <div
      className={`${styles.tile} ${offline ? styles.tileOffline : ""}`}
      style={{ borderColor: statusColor(m.status) }}
      onClick={handleClick}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      title={`SSH: ${m.host}:${m.port}`}
    >
      {/* Header */}
      <div className={styles.tileHeader}>
        <StatusDot status={m.status} />
        <span className={styles.machineName}>{m.name}</span>
        <span className={styles.machineType}>{m.type.toUpperCase()}</span>
      </div>

      {/* Offline overlay */}
      {offline && (
        <div className={styles.offlineOverlay}>
          <span>OFFLINE</span>
          {m.lastSeen && (
            <span className={styles.offlineDetail}>
              last seen {new Date(m.lastSeen).toLocaleTimeString()}
            </span>
          )}
        </div>
      )}

      {/* Metrics */}
      {!offline && (
        <>
          <MetricBar label="CPU" value={m.cpu} />
          <MetricBar label="MEM" value={m.memory} warnAt={85} critAt={95} />
          <MetricBar label="DSK" value={m.disk} warnAt={80} critAt={90} />

          {/* GPU — Linux DGX machines */}
          {m.gpus && m.gpus.length > 0 && (
            <MetricBar
              label={`GPU×${m.gpus.length}`}
              value={gpuAvg}
            />
          )}

          {/* Footer row */}
          <div className={styles.tileFooter}>
            {m.docker && (
              <span className={styles.badge}>
                Docker {m.docker.running}/{m.docker.total}
              </span>
            )}
            {m.mikrotik && (
              <>
                <span className={styles.badge}>Router</span>
                <span className={styles.badge}>
                  WiFi {m.mikrotik.wifiClients} cl
                </span>
              </>
            )}
            <span className={styles.uptime}>up {fmtUptime(m.uptime)}</span>
          </div>
        </>
      )}

      {/* Hover detail panel */}
      {hovered && !offline && (
        <div className={styles.detailPanel}>
          <table className={styles.detailTable}>
            <tbody>
              <tr><td>Host</td><td>{m.host}:{m.port}</td></tr>
              <tr><td>CPU</td><td>{m.cpu.toFixed(1)}%</td></tr>
              <tr><td>Memory</td><td>{m.memory.toFixed(1)}%</td></tr>
              <tr><td>Disk</td><td>{m.disk.toFixed(1)}%</td></tr>
              {m.gpus && m.gpus.map((g) => (
                <tr key={g.index}>
                  <td>GPU {g.index}</td>
                  <td>
                    util {g.utilPercent.toFixed(0)}%  mem {g.memPercent.toFixed(0)}%
                    {" "}{g.tempC.toFixed(0)}°C
                  </td>
                </tr>
              ))}
              {m.docker && (
                <tr><td>Docker</td><td>{m.docker.running} running / {m.docker.total} total</td></tr>
              )}
              {m.mikrotik && (
                <>
                  <tr><td>WiFi clients</td><td>{m.mikrotik.wifiClients}</td></tr>
                  <tr><td>ETH rx</td><td>{fmtBytes(m.mikrotik.ethRxBytes)}</td></tr>
                  <tr><td>ETH tx</td><td>{fmtBytes(m.mikrotik.ethTxBytes)}</td></tr>
                </>
              )}
              <tr><td>Uptime</td><td>{fmtUptime(m.uptime)}</td></tr>
            </tbody>
          </table>
          <div className={styles.sshHint}>Click to SSH</div>
        </div>
      )}
    </div>
  );
}

// ─── Dashboard ───────────────────────────────────────────────────────────────

export default function MachineHealthDashboard({
  machines: machinesProp,
  apiUrl = "/api/status",
  pollIntervalMs = 15000,
  onSSHClick,
}: DashboardProps) {
  const [machines, setMachines] = useState<MachineStatus[]>(machinesProp ?? []);
  const [lastRefresh, setLastRefresh] = useState<Date>(new Date());
  const [error, setError] = useState<string>("");

  // Auto-poll if no machines prop
  useEffect(() => {
    if (machinesProp) {
      setMachines(machinesProp);
      return;
    }

    const fetchStatus = async () => {
      try {
        const resp = await fetch(apiUrl);
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const data: MachineStatus[] = await resp.json();
        setMachines(data);
        setLastRefresh(new Date());
        setError("");
      } catch (e) {
        setError(String(e));
      }
    };

    fetchStatus();
    const id = setInterval(fetchStatus, pollIntervalMs);
    return () => clearInterval(id);
  }, [machinesProp, apiUrl, pollIntervalMs]);

  const summary = {
    ok:       machines.filter((m) => m.status === "ok").length,
    warning:  machines.filter((m) => m.status === "warning").length,
    critical: machines.filter((m) => m.status === "critical").length,
    unknown:  machines.filter((m) => m.status === "unknown").length,
  };

  return (
    <div className={styles.dashboard}>
      {/* Top bar */}
      <div className={styles.topBar}>
        <span className={styles.title}>Machine Health</span>
        <div className={styles.summaryBadges}>
          {summary.ok > 0 && (
            <span className={styles.badgeOk}>{summary.ok} OK</span>
          )}
          {summary.warning > 0 && (
            <span className={styles.badgeWarn}>{summary.warning} WARN</span>
          )}
          {summary.critical > 0 && (
            <span className={styles.badgeCrit}>{summary.critical} CRIT</span>
          )}
          {summary.unknown > 0 && (
            <span className={styles.badgeUnk}>{summary.unknown} DOWN</span>
          )}
        </div>
        <span className={styles.refreshTime}>
          {lastRefresh.toLocaleTimeString()}
        </span>
      </div>

      {error && (
        <div className={styles.errorBar}>Poll error: {error}</div>
      )}

      {/* Tile grid */}
      <div className={styles.grid}>
        {machines.map((m) => (
          <MachineTile key={m.name} machine={m} onSSHClick={onSSHClick} />
        ))}
        {machines.length === 0 && (
          <div className={styles.empty}>No machines configured.</div>
        )}
      </div>
    </div>
  );
}
