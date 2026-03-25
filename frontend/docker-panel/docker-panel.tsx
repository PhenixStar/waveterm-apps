/**
 * docker-panel.tsx — Docker orchestration panel for Wave Terminal WaveApp
 *
 * Connects to docker-backend.go at http://localhost:9173
 * Features: multi-host container table, live stats, log viewer, ANSI colors
 *
 * Dependencies:
 *   npm install ansi-to-html dompurify
 *   npm install @types/react @types/dompurify (React 18+)
 */

import React, {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';

// ─────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────

interface Container {
  ID: string;
  Names: string;
  Image: string;
  Status: string;
  State: string;
  Ports: string;
  CreatedAt: string;
  RunningFor: string;
  host: string;
}

interface ContainerStats {
  Name: string;
  CPUPerc: string;
  MemUsage: string;
  MemPerc: string;
  NetIO: string;
  BlockIO: string;
  PIDs: string;
  Container: string;
  host: string;
}

interface Host {
  name: string;
  label: string;
  reachable: boolean;
}

type SortKey = 'Names' | 'Image' | 'State' | 'host' | 'CPUPerc' | 'MemPerc';
type StateFilter = 'all' | 'running' | 'exited';

// ─────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────

const API_BASE = 'http://localhost:9173';
const POLL_INTERVAL = 10_000; // ms

const STATE_COLORS: Record<string, string> = {
  running: '#22c55e',
  exited:  '#6b7280',
  paused:  '#f59e0b',
  created: '#3b82f6',
  dead:    '#ef4444',
};

// ─────────────────────────────────────────────
// ANSI decoder — convert ANSI escape codes to safe HTML
// Uses ansi-to-html for conversion + DOMPurify for sanitization.
// Log lines come from docker logs (trusted source — same host we SSH'd into),
// but we still sanitize as defense-in-depth.
// ─────────────────────────────────────────────

let _ansiConverter: { convert: (s: string) => string } | null = null;
let _dompurify: { sanitize: (s: string) => string } | null = null;

function initDeps(): void {
  if (!_ansiConverter) {
    try {
      const AnsiToHtml = require('ansi-to-html');
      _ansiConverter = new AnsiToHtml({ fg: '#e2e8f0', bg: '#0f172a', newline: false });
    } catch {
      _ansiConverter = { convert: (s: string) => s.replace(/\x1b\[[0-9;]*[mGKH]/g, '') };
    }
  }
  if (!_dompurify) {
    try {
      const DOMPurify = require('dompurify');
      _dompurify = { sanitize: (s: string) => DOMPurify.sanitize(s, { ALLOWED_TAGS: ['span', 'b', 'i'], ALLOWED_ATTR: ['style'] }) };
    } catch {
      // Fallback: strip all tags — safe, loses color but never unsafe
      _dompurify = { sanitize: (s: string) => s.replace(/<[^>]*>/g, '') };
    }
  }
}

function ansiToSafeHtml(text: string): string {
  initDeps();
  const withHtml = _ansiConverter!.convert(text);
  return _dompurify!.sanitize(withHtml);
}

// ─────────────────────────────────────────────
// API helpers
// ─────────────────────────────────────────────

async function fetchJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`);
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error((err as Record<string, string>).error ?? res.statusText);
  }
  return res.json();
}

async function postAction(host: string, id: string, action: string): Promise<void> {
  const method = action === 'remove' ? 'DELETE' : 'POST';
  const path = action === 'remove'
    ? `/api/containers/${host}/${id}`
    : `/api/containers/${host}/${id}/${action}`;

  const res = await fetch(`${API_BASE}${path}`, { method });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error((err as Record<string, string>).error ?? res.statusText);
  }
}

// ─────────────────────────────────────────────
// Sub-components
// ─────────────────────────────────────────────

// StatusBadge
const StatusBadge: React.FC<{ state: string }> = ({ state }) => (
  <span
    style={{
      display: 'inline-flex',
      alignItems: 'center',
      gap: 4,
      fontSize: 10,
      textTransform: 'uppercase',
      letterSpacing: '0.06em',
      color: STATE_COLORS[state] ?? '#94a3b8',
      fontFamily: 'ui-monospace, monospace',
    }}
  >
    <span
      style={{
        width: 6,
        height: 6,
        borderRadius: '50%',
        background: STATE_COLORS[state] ?? '#94a3b8',
        boxShadow: state === 'running' ? `0 0 4px ${STATE_COLORS.running}` : undefined,
      }}
    />
    {state}
  </span>
);

// MiniBar — inline percentage bar
const MiniBar: React.FC<{ pct: number; color: string }> = ({ pct, color }) => (
  <div
    style={{
      width: 48,
      height: 6,
      background: '#1e293b',
      borderRadius: 3,
      overflow: 'hidden',
    }}
  >
    <div
      style={{
        width: `${Math.min(100, pct)}%`,
        height: '100%',
        background: color,
        borderRadius: 3,
        transition: 'width 0.5s ease',
      }}
    />
  </div>
);

function parsePercent(s: string | undefined): number {
  return parseFloat(s?.replace('%', '') ?? '0') || 0;
}

// ConfirmDialog
interface ConfirmDialogProps {
  message: string;
  onConfirm: () => void;
  onCancel: () => void;
}

const ConfirmDialog: React.FC<ConfirmDialogProps> = ({ message, onConfirm, onCancel }) => (
  <div
    style={{
      position: 'fixed',
      inset: 0,
      background: 'rgba(0,0,0,0.6)',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      zIndex: 200,
    }}
  >
    <div
      style={{
        background: '#0f172a',
        border: '1px solid #334155',
        borderRadius: 8,
        padding: '24px 32px',
        maxWidth: 380,
        color: '#e2e8f0',
        fontFamily: 'ui-monospace, monospace',
        fontSize: 13,
      }}
    >
      <div style={{ marginBottom: 20, color: '#f8fafc' }}>{message}</div>
      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
        <button onClick={onCancel} style={tabStyle}>Cancel</button>
        <button
          onClick={onConfirm}
          style={{ ...tabStyle, background: '#7f1d1d', borderColor: '#ef4444', color: '#fca5a5' }}
        >
          Confirm
        </button>
      </div>
    </div>
  </div>
);

// LogViewer — SSE-based streaming log panel
interface LogViewerProps {
  container: Container;
  onClose: () => void;
}

const LogViewer: React.FC<LogViewerProps> = ({ container, onClose }) => {
  const [lines, setLines] = useState<string[]>([]);
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const url = `${API_BASE}/api/containers/${container.host}/${container.ID}/logs/stream?tail=200`;
    const es = new EventSource(url);

    es.onmessage = (e: MessageEvent<string>) => {
      // e.data is a raw log line from docker — treat as plain text,
      // convert ANSI codes, sanitize before rendering.
      setLines(prev => {
        const next = [...prev, e.data];
        return next.length > 1000 ? next.slice(-1000) : next;
      });
    };

    es.onerror = () => {
      setLines(prev => [...prev, '[log stream disconnected]']);
    };

    return () => { es.close(); };
  }, [container.host, container.ID]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [lines]);

  return (
    <div
      style={{
        position: 'fixed',
        inset: 0,
        background: '#0a0f1a',
        display: 'flex',
        flexDirection: 'column',
        zIndex: 150,
        fontFamily: 'ui-monospace, "Cascadia Code", monospace',
      }}
    >
      {/* Header */}
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          padding: '10px 16px',
          borderBottom: '1px solid #1e293b',
          background: '#0f172a',
          flexShrink: 0,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ color: '#22c55e', fontSize: 12 }}>&#9679;</span>
          <span style={{ color: '#f8fafc', fontWeight: 600, fontSize: 12 }}>
            {container.Names}
          </span>
          <span style={{ color: '#475569', fontSize: 11 }}>
            [{container.host}] {container.ID.slice(0, 12)}
          </span>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={() => setLines([])} style={tabStyle}>Clear</button>
          <button onClick={onClose} style={tabStyle}>Close</button>
        </div>
      </div>

      {/* Log lines — ANSI-decoded + DOMPurify-sanitized */}
      <div style={{ flex: 1, overflow: 'auto', padding: '8px 16px', fontSize: 11, lineHeight: 1.6 }}>
        {lines.length === 0 && (
          <div style={{ color: '#475569' }}>Waiting for logs...</div>
        )}
        {lines.map((line, i) => (
          <div
            key={i}
            // Safe: ansiToSafeHtml applies DOMPurify with ALLOWED_TAGS whitelist (span,b,i only)
            // Source is docker log output from our own SSH-controlled hosts.
            dangerouslySetInnerHTML={{ __html: ansiToSafeHtml(line) }} // eslint-disable-line react/no-danger
            style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: '#e2e8f0' }}
          />
        ))}
        <div ref={bottomRef} />
      </div>
    </div>
  );
};

// ─────────────────────────────────────────────
// Main Component
// ─────────────────────────────────────────────

export const DockerPanel: React.FC = () => {
  const [containers, setContainers] = useState<Container[]>([]);
  const [statsMap, setStatsMap] = useState<Record<string, ContainerStats>>({});
  const [hosts, setHosts] = useState<Host[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [hostFilter, setHostFilter] = useState<string>('all');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [searchQuery, setSearchQuery] = useState('');
  const [sortKey, setSortKey] = useState<SortKey>('Names');
  const [sortAsc, setSortAsc] = useState(true);
  const [selectedContainer, setSelectedContainer] = useState<Container | null>(null);
  const [logViewerContainer, setLogViewerContainer] = useState<Container | null>(null);
  const [confirmAction, setConfirmAction] = useState<{ container: Container; action: 'stop' | 'remove' } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  // ── Fetch ──────────────────────────────────
  const fetchData = useCallback(async () => {
    try {
      const [cs, ss, hs] = await Promise.all([
        fetchJSON<Container[]>('/api/containers'),
        fetchJSON<ContainerStats[]>('/api/stats'),
        fetchJSON<Host[]>('/api/hosts'),
      ]);

      setContainers(cs ?? []);
      setHosts(hs ?? []);

      const map: Record<string, ContainerStats> = {};
      for (const s of (ss ?? [])) {
        map[`${s.host}:${s.Name}`] = s;
      }
      setStatsMap(map);
      setError(null);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchData();
    const id = setInterval(fetchData, POLL_INTERVAL);
    return () => clearInterval(id);
  }, [fetchData]);

  // ── Filtered + sorted ─────────────────────
  const filtered = useMemo(() => {
    let list = containers;
    if (hostFilter !== 'all') list = list.filter(c => c.host === hostFilter);
    if (stateFilter !== 'all') list = list.filter(c => c.State === stateFilter);
    if (searchQuery) {
      const q = searchQuery.toLowerCase();
      list = list.filter(c =>
        c.Names.toLowerCase().includes(q) || c.Image.toLowerCase().includes(q)
      );
    }

    return [...list].sort((a, b) => {
      if (sortKey === 'CPUPerc' || sortKey === 'MemPerc') {
        const as = statsMap[`${a.host}:${a.Names}`];
        const bs = statsMap[`${b.host}:${b.Names}`];
        const ap = parsePercent(sortKey === 'CPUPerc' ? as?.CPUPerc : as?.MemPerc);
        const bp = parsePercent(sortKey === 'CPUPerc' ? bs?.CPUPerc : bs?.MemPerc);
        return sortAsc ? ap - bp : bp - ap;
      }
      const av = (a as Record<string, string>)[sortKey] ?? '';
      const bv = (b as Record<string, string>)[sortKey] ?? '';
      return sortAsc ? av.localeCompare(bv) : bv.localeCompare(av);
    });
  }, [containers, hostFilter, stateFilter, searchQuery, sortKey, sortAsc, statsMap]);

  // ── Actions ────────────────────────────────
  const handleAction = useCallback(async (container: Container, action: string) => {
    if (action === 'logs') { setLogViewerContainer(container); return; }
    if (action === 'inspect') {
      window.open(`${API_BASE}/api/containers/${container.host}/${container.ID}/inspect`, '_blank');
      return;
    }
    if (action === 'stop' || action === 'remove') {
      setConfirmAction({ container, action });
      return;
    }
    try {
      setActionError(null);
      await postAction(container.host, container.ID, action);
      await fetchData();
    } catch (e: unknown) {
      setActionError(e instanceof Error ? e.message : String(e));
    }
  }, [fetchData]);

  const handleConfirm = useCallback(async () => {
    if (!confirmAction) return;
    try {
      setActionError(null);
      await postAction(confirmAction.container.host, confirmAction.container.ID, confirmAction.action);
      await fetchData();
    } catch (e: unknown) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setConfirmAction(null);
    }
  }, [confirmAction, fetchData]);

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) setSortAsc(v => !v);
    else { setSortKey(key); setSortAsc(true); }
  };

  // ── Render ────────────────────────────────

  if (logViewerContainer) {
    return <LogViewer container={logViewerContainer} onClose={() => setLogViewerContainer(null)} />;
  }

  const colHdr = (key: SortKey, label: string) => (
    <Th
      style={{ cursor: 'pointer', color: sortKey === key ? '#06b6d4' : '#64748b', fontWeight: sortKey === key ? 700 : 400 }}
      onClick={() => toggleSort(key)}
    >
      {label} {sortKey === key ? (sortAsc ? '▲' : '▼') : ''}
    </Th>
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#0a0f1a', color: '#e2e8f0', fontFamily: 'ui-monospace, monospace', fontSize: 12 }}>

      {/* Toolbar */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '8px 12px', borderBottom: '1px solid #1e293b', background: '#0f172a', flexShrink: 0, flexWrap: 'wrap' }}>
        <button onClick={() => setHostFilter('all')} style={hostFilter === 'all' ? activeTabStyle : tabStyle}>
          All ({containers.length})
        </button>
        {hosts.map(h => (
          <button key={h.name} onClick={() => setHostFilter(h.name)} style={hostFilter === h.name ? activeTabStyle : tabStyle}>
            <span style={{ width: 6, height: 6, borderRadius: '50%', background: h.reachable ? '#22c55e' : '#ef4444', display: 'inline-block', marginRight: 4 }} />
            {h.label}
          </button>
        ))}
        <div style={{ display: 'flex', gap: 4, marginLeft: 'auto' }}>
          {(['all', 'running', 'exited'] as StateFilter[]).map(s => (
            <button key={s} onClick={() => setStateFilter(s)} style={stateFilter === s ? activeTabStyle : tabStyle}>{s}</button>
          ))}
        </div>
        <input
          type="text"
          placeholder="Search..."
          value={searchQuery}
          onChange={e => setSearchQuery(e.target.value)}
          style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: 4, padding: '4px 8px', color: '#e2e8f0', fontSize: 11, width: 160, outline: 'none' }}
        />
        <button onClick={fetchData} style={tabStyle} title="Refresh">&#8635;</button>
      </div>

      {/* Error bar */}
      {(error || actionError) && (
        <div style={{ padding: '6px 12px', background: '#450a0a', color: '#fca5a5', fontSize: 11, borderBottom: '1px solid #7f1d1d', flexShrink: 0 }}>
          {error || actionError}
        </div>
      )}

      {/* Table */}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {loading ? (
          <div style={{ padding: 24, color: '#475569' }}>Connecting to backend at {API_BASE}...</div>
        ) : (
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid #1e293b', background: '#0f172a' }}>
                {colHdr('Names', 'Name')}
                {colHdr('Image', 'Image')}
                {colHdr('State', 'Status')}
                {colHdr('CPUPerc', 'CPU')}
                {colHdr('MemPerc', 'Mem')}
                <Th>Ports</Th>
                {colHdr('host', 'Host')}
                <Th>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {filtered.length === 0 && (
                <tr><td colSpan={8} style={{ padding: 16, color: '#475569', textAlign: 'center' }}>No containers match filters</td></tr>
              )}
              {filtered.map(c => {
                const stats = statsMap[`${c.host}:${c.Names}`];
                const cpu = parsePercent(stats?.CPUPerc);
                const mem = parsePercent(stats?.MemPerc);
                const isSelected = selectedContainer?.ID === c.ID && selectedContainer?.host === c.host;

                return (
                  <tr
                    key={`${c.host}:${c.ID}`}
                    onClick={() => setSelectedContainer(isSelected ? null : c)}
                    style={{ borderBottom: '1px solid #1e293b', background: isSelected ? '#1e293b' : 'transparent', cursor: 'pointer' }}
                  >
                    <Td style={{ color: '#f8fafc', maxWidth: 160, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {c.Names.replace(/^\//, '')}
                    </Td>
                    <Td style={{ color: '#94a3b8', maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {c.Image}
                    </Td>
                    <Td><StatusBadge state={c.State} /></Td>
                    <Td>
                      {stats
                        ? <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}><span style={{ color: cpu > 80 ? '#ef4444' : '#94a3b8' }}>{stats.CPUPerc}</span><MiniBar pct={cpu} color={cpu > 80 ? '#ef4444' : '#06b6d4'} /></div>
                        : <span style={{ color: '#334155' }}>—</span>}
                    </Td>
                    <Td>
                      {stats
                        ? <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}><span style={{ color: mem > 80 ? '#f59e0b' : '#94a3b8' }}>{stats.MemPerc}</span><MiniBar pct={mem} color={mem > 80 ? '#f59e0b' : '#8b5cf6'} /></div>
                        : <span style={{ color: '#334155' }}>—</span>}
                    </Td>
                    <Td style={{ color: '#475569', fontSize: 10, maxWidth: 120, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.Ports || '—'}</Td>
                    <Td style={{ color: '#06b6d4' }}>{c.host}</Td>
                    <Td>
                      <div style={{ display: 'flex', gap: 4 }} onClick={e => e.stopPropagation()}>
                        {c.State !== 'running' && <ActionBtn color="#22c55e" onClick={() => handleAction(c, 'start')} title="Start">&#9654;</ActionBtn>}
                        {c.State === 'running' && <ActionBtn color="#f59e0b" onClick={() => handleAction(c, 'stop')} title="Stop">&#9632;</ActionBtn>}
                        <ActionBtn color="#3b82f6" onClick={() => handleAction(c, 'restart')} title="Restart">&#8635;</ActionBtn>
                        <ActionBtn color="#8b5cf6" onClick={() => handleAction(c, 'logs')} title="Logs">&#8801;</ActionBtn>
                        <ActionBtn color="#64748b" onClick={() => handleAction(c, 'inspect')} title="Inspect">&#9741;</ActionBtn>
                        <ActionBtn color="#ef4444" onClick={() => handleAction(c, 'remove')} title="Remove">&#10005;</ActionBtn>
                      </div>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Detail panel */}
      {selectedContainer && (
        <ContainerDetail
          container={selectedContainer}
          stats={statsMap[`${selectedContainer.host}:${selectedContainer.Names}`]}
          onClose={() => setSelectedContainer(null)}
        />
      )}

      {confirmAction && (
        <ConfirmDialog
          message={`${confirmAction.action === 'stop' ? 'Stop' : 'Remove'} container "${confirmAction.container.Names}"?${confirmAction.action === 'remove' ? ' This cannot be undone.' : ''}`}
          onConfirm={handleConfirm}
          onCancel={() => setConfirmAction(null)}
        />
      )}
    </div>
  );
};

// ─────────────────────────────────────────────
// ContainerDetail
// ─────────────────────────────────────────────

const ContainerDetail: React.FC<{ container: Container; stats?: ContainerStats; onClose: () => void }> = ({ container, stats, onClose }) => (
  <div style={{ borderTop: '1px solid #1e293b', background: '#0f172a', padding: '12px 16px', flexShrink: 0, display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: '8px 16px', fontSize: 11, maxHeight: 160, overflow: 'auto' }}>
    <div>
      <div style={{ color: '#475569', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>Container</div>
      <div style={{ color: '#f8fafc', fontWeight: 600 }}>{container.Names.replace(/^\//, '')}</div>
      <div style={{ color: '#94a3b8' }}>{container.ID.slice(0, 12)}</div>
      <div style={{ color: '#475569', marginTop: 2 }}>{container.Image}</div>
    </div>
    <div>
      <div style={{ color: '#475569', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>Network</div>
      <div style={{ color: '#94a3b8' }}>{container.Ports || 'No ports'}</div>
      <div style={{ color: '#475569', marginTop: 2 }}>Host: {container.host}</div>
    </div>
    {stats && (
      <div>
        <div style={{ color: '#475569', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>Resources</div>
        <div>CPU: <span style={{ color: '#06b6d4' }}>{stats.CPUPerc}</span></div>
        <div>Mem: <span style={{ color: '#8b5cf6' }}>{stats.MemUsage}</span> ({stats.MemPerc})</div>
        <div>Net I/O: <span style={{ color: '#94a3b8' }}>{stats.NetIO}</span></div>
        <div>PIDs: <span style={{ color: '#94a3b8' }}>{stats.PIDs}</span></div>
      </div>
    )}
    <div>
      <div style={{ color: '#475569', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>Info</div>
      <div style={{ color: '#94a3b8' }}>Created: {container.CreatedAt}</div>
      <div style={{ color: '#94a3b8' }}>{container.RunningFor}</div>
    </div>
    <div style={{ display: 'flex', alignItems: 'flex-end' }}>
      <button onClick={onClose} style={{ ...tabStyle, marginLeft: 'auto' }}>Close</button>
    </div>
  </div>
);

// ─────────────────────────────────────────────
// Style helpers
// ─────────────────────────────────────────────

const tabStyle: React.CSSProperties = {
  background: '#1e293b',
  border: '1px solid #334155',
  color: '#94a3b8',
  borderRadius: 4,
  padding: '4px 10px',
  cursor: 'pointer',
  fontSize: 11,
  fontFamily: 'inherit',
};

const activeTabStyle: React.CSSProperties = {
  ...tabStyle,
  background: '#1e3a5f',
  borderColor: '#06b6d4',
  color: '#06b6d4',
};

const Th: React.FC<React.ThHTMLAttributes<HTMLTableCellElement>> = ({ children, style, ...rest }) => (
  <th style={{ padding: '6px 10px', textAlign: 'left', fontWeight: 500, fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.05em', whiteSpace: 'nowrap', ...style }} {...rest}>
    {children}
  </th>
);

const Td: React.FC<React.TdHTMLAttributes<HTMLTableCellElement>> = ({ children, style, ...rest }) => (
  <td style={{ padding: '7px 10px', verticalAlign: 'middle', ...style }} {...rest}>
    {children}
  </td>
);

const ActionBtn: React.FC<{ color: string; onClick: () => void; title: string; children: React.ReactNode }> = ({ color, onClick, title, children }) => (
  <button
    onClick={onClick}
    title={title}
    style={{ background: 'transparent', border: `1px solid ${color}44`, color, borderRadius: 3, padding: '2px 6px', cursor: 'pointer', fontSize: 11, lineHeight: 1 }}
    onMouseEnter={e => (e.currentTarget.style.background = `${color}22`)}
    onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
  >
    {children}
  </button>
);

export default DockerPanel;
