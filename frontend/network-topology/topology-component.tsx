/**
 * NetworkTopology.tsx — Interactive VLAN-grouped network topology for WaveApp
 *
 * Renders 15+ nodes (routers, servers, laptops) with:
 * - Color-coded VLAN clusters
 * - Live status indicators (online/offline/warning)
 * - Click-to-select with detail panel
 * - Responsive SVG with zoom/pan
 * - Real-time WebSocket status updates (optional)
 *
 * Requirements: d3, React 18+
 *   npm install d3 @types/d3
 */

import React, { useEffect, useRef, useState, useCallback, useMemo } from 'react';
import * as d3 from 'd3';

// ─────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────

type NodeStatus = 'online' | 'offline' | 'warning';
type NodeType = 'router' | 'server' | 'laptop' | 'switch' | 'firewall' | 'cloud';

interface NetworkNode {
  id: string;
  label: string;
  type: NodeType;
  vlan: number;
  ip?: string;
  mac?: string;
  status: NodeStatus;
  metadata?: Record<string, string>;
}

interface NetworkLink {
  id: string;
  source: string;
  target: string;
  label?: string;
  bandwidth?: string;
  status: 'active' | 'inactive' | 'warning';
}

interface TopologyData {
  nodes: NetworkNode[];
  links: NetworkLink[];
}

// ─────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────

const VLAN_COLORS: Record<number, string> = {
  100: '#3b82f6', // LAN      — blue
  101: '#8b5cf6', // Corp     — violet
  102: '#06b6d4', // CC       — cyan
  103: '#f59e0b', // Crypto   — amber
  104: '#ec4899', // Media    — pink
  105: '#10b981', // POS      — emerald
  106: '#6366f1', // Staff    — indigo
  40:  '#ef4444', // CCTV     — red
  50:  '#84cc16', // Guest-ATL— lime
  60:  '#14b8a6', // Guest-WiFi — teal
  110: '#f97316', // Management — orange
};

const STATUS_COLORS: Record<NodeStatus, string> = {
  online:  '#22c55e',
  offline: '#6b7280',
  warning: '#f59e0b',
};

const NODE_ICONS: Record<NodeType, string> = {
  router:  '\u{1F310}',   // 🌐
  server:  '\u{1F4BB}',   // 💻
  laptop:  '\u{1F4F1}',   // 📱
  switch:  '\u{1F50C}',   // 🔌
  firewall:'\u{1F6E1}',   // 🛡️
  cloud:   '\u{2601}',    // ☁️
};

const NODE_SIZE = 32;
const VLAN_CLUSTER_PADDING = 24;

// ─────────────────────────────────────────────────────────────
// Sub-components
// ─────────────────────────────────────────────────────────────

interface DetailPanelProps {
  node: NetworkNode | null;
  links: NetworkLink[];
  allNodes: NetworkNode[];
  onClose: () => void;
}

const DetailPanel: React.FC<DetailPanelProps> = ({ node, links, allNodes, onClose }) => {
  if (!node) return null;

  const connectedLinks = links.filter(l => l.source === node.id || l.target === node.id);
  const connectedNodes = connectedLinks.map(l => {
    const tid = l.source === node.id ? l.target : l.source;
    return allNodes.find(n => n.id === tid);
  }).filter(Boolean) as NetworkNode[];

  const vlanColor = VLAN_COLORS[node.vlan] ?? '#9ca3af';

  return (
    <div
      style={{
        position: 'absolute',
        top: 16,
        right: 16,
        width: 280,
        background: 'rgba(15,23,42,0.95)',
        border: `1px solid ${vlanColor}`,
        borderRadius: 8,
        padding: 16,
        color: '#e2e8f0',
        fontFamily: 'ui-monospace, monospace',
        fontSize: 12,
        boxShadow: `0 4px 24px rgba(0,0,0,0.5), 0 0 0 1px ${vlanColor}22`,
        backdropFilter: 'blur(8px)',
        zIndex: 100,
      }}
    >
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 12 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ fontSize: 20 }}>{NODE_ICONS[node.type]}</span>
          <div>
            <div style={{ fontWeight: 700, color: '#f8fafc' }}>{node.label}</div>
            <div style={{ color: vlanColor, fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              VLAN {node.vlan}
            </div>
          </div>
        </div>
        <button
          onClick={onClose}
          style={{
            background: 'transparent',
            border: 'none',
            color: '#94a3b8',
            cursor: 'pointer',
            fontSize: 16,
            padding: 2,
            lineHeight: 1,
          }}
        >
          ✕
        </button>
      </div>

      {/* Status badge */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 12 }}>
        <span
          style={{
            width: 8,
            height: 8,
            borderRadius: '50%',
            background: STATUS_COLORS[node.status],
            boxShadow: `0 0 6px ${STATUS_COLORS[node.status]}`,
            animation: node.status === 'online' ? 'pulse 2s infinite' : undefined,
          }}
        />
        <span style={{ textTransform: 'uppercase', fontSize: 10, letterSpacing: '0.1em', color: STATUS_COLORS[node.status] }}>
          {node.status}
        </span>
      </div>

      {/* Info grid */}
      <div style={{ display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '4px 12px', marginBottom: 12 }}>
        <span style={{ color: '#64748b' }}>Type</span>
        <span style={{ textTransform: 'capitalize' }}>{node.type}</span>
        <span style={{ color: '#64748b' }}>IP</span>
        <span>{node.ip ?? '—'}</span>
        {node.mac && (
          <>
            <span style={{ color: '#64748b' }}>MAC</span>
            <span style={{ fontFamily: 'ui-monospace', fontSize: 10 }}>{node.mac}</span>
          </>
        )}
      </div>

      {/* Metadata */}
      {node.metadata && Object.keys(node.metadata).length > 0 && (
        <div style={{ marginBottom: 12 }}>
          <div style={{ color: '#64748b', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>
            Metadata
          </div>
          {Object.entries(node.metadata).map(([k, v]) => (
            <div key={k} style={{ display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '2px 8px' }}>
              <span style={{ color: '#64748b' }}>{k}</span>
              <span>{v}</span>
            </div>
          ))}
        </div>
      )}

      {/* Connected nodes */}
      {connectedNodes.length > 0 && (
        <div>
          <div style={{ color: '#64748b', marginBottom: 4, fontSize: 10, textTransform: 'uppercase' }}>
            Connected ({connectedNodes.length})
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {connectedNodes.map(cn => (
              <div
                key={cn.id}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 6,
                  padding: '4px 6px',
                  background: 'rgba(255,255,255,0.04)',
                  borderRadius: 4,
                }}
              >
                <span>{NODE_ICONS[cn.type]}</span>
                <span style={{ flex: 1 }}>{cn.label}</span>
                <span style={{ color: VLAN_COLORS[cn.vlan] ?? '#9ca3af', fontSize: 10 }}>
                  VLAN{cn.vlan}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* CSS animation */}
      <style>{`
        @keyframes pulse {
          0%, 100% { opacity: 1; }
          50% { opacity: 0.5; }
        }
      `}</style>
    </div>
  );
};

// ─────────────────────────────────────────────────────────────
// VLAN Legend
// ─────────────────────────────────────────────────────────────

interface VlanLegendProps {
  vlans: number[];
}

const VlanLegend: React.FC<VlanLegendProps> = ({ vlans }) => (
  <div
    style={{
      position: 'absolute',
      bottom: 16,
      left: 16,
      background: 'rgba(15,23,42,0.9)',
      border: '1px solid rgba(255,255,255,0.1)',
      borderRadius: 6,
      padding: '8px 12px',
      display: 'flex',
      flexWrap: 'wrap',
      gap: '8px 16px',
      maxWidth: 400,
      fontFamily: 'ui-monospace, monospace',
      fontSize: 10,
    }}
  >
    {vlans.map(vlan => (
      <div key={vlan} style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
        <span
          style={{
            width: 10,
            height: 10,
            borderRadius: 2,
            background: VLAN_COLORS[vlan] ?? '#9ca3af',
          }}
        />
        <span style={{ color: '#94a3b8' }}>VLAN {vlan}</span>
      </div>
    ))}
  </div>
);

// ─────────────────────────────────────────────────────────────
// Main Component
// ─────────────────────────────────────────────────────────────

interface NetworkTopologyProps {
  /** Initial topology data */
  data?: TopologyData;
  /** WebSocket URL for live updates (optional) */
  wsUrl?: string;
  /** Called when a node is clicked */
  onNodeClick?: (node: NetworkNode) => void;
  /** Called when link is clicked */
  onLinkClick?: (link: NetworkLink) => void;
}

export const NetworkTopology: React.FC<NetworkTopologyProps> = ({
  data,
  wsUrl,
  onNodeClick,
  onLinkClick,
}) => {
  const svgRef = useRef<SVGSVGElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [selectedNode, setSelectedNode] = useState<NetworkNode | null>(null);
  const [width, setWidth] = useState(800);
  const [height, setHeight] = useState(600);
  const simulationRef = useRef<d3.Simulation<NetworkNode, NetworkLink> | null>(null);

  // Demo data fallback
  const topology = useMemo<TopologyData>(() => data ?? DEMO_DATA, [data]);

  // ── Responsive resize ──────────────────────────────────────
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const observer = new ResizeObserver(entries => {
      const { width: w, height: h } = entries[0].contentRect;
      setWidth(w);
      setHeight(h);
    });

    observer.observe(container);
    return () => observer.disconnect();
  }, []);

  // ── D3 Rendering ──────────────────────────────────────────
  useEffect(() => {
    const svg = d3.select(svgRef.current);
    if (!svg || width === 0 || height === 0) return;

    // Clear previous render
    svg.selectAll('*').remove();

    // ── Defs (gradients, filters) ──
    const defs = svg.append('defs');

    // Glow filter for online nodes
    const glowFilter = defs.append('filter').attr('id', 'glow').attr('x', '-50%').attr('y', '-50%').attr('width', '200%').attr('height', '200%');
    glowFilter.append('feGaussianBlur').attr('stdDeviation', 3).attr('result', 'coloredBlur');
    const feMerge = glowFilter.append('feMerge');
    feMerge.append('feMergeNode').attr('in', 'coloredBlur');
    feMerge.append('feMergeNode').attr('in', 'SourceGraphic');

    // Warning pulse filter
    const warningFilter = defs.append('filter').attr('id', 'warning-glow');
    warningFilter.append('feGaussianBlur').attr('stdDeviation', 4).attr('result', 'blur');
    const wfMerge = warningFilter.append('feMerge');
    wfMerge.append('feMergeNode').attr('in', 'blur');
    wfMerge.append('feMergeNode').attr('in', 'SourceGraphic');

    // ── Root group (zoom target) ──
    const root = svg.append('g').attr('class', 'root');

    // ── Zoom behavior ──
    const zoom = d3.zoom<SVGSVGElement, unknown>()
      .scaleExtent([0.1, 4])
      .on('zoom', event => {
        root.attr('transform', event.transform);
      });

    svg.call(zoom);
    svg.on('dblclick.zoom', () => {
      svg.transition().duration(500).call(zoom.transform, d3.zoomIdentity);
    });

    // ── Clone nodes/links for simulation ──
    const nodes: NetworkNode[] = topology.nodes.map(n => ({ ...n }));
    const nodeMap = new Map(nodes.map(n => [n.id, n]));
    const links: NetworkLink[] = topology.links
      .filter(l => nodeMap.has(l.source) && nodeMap.has(l.target))
      .map(l => ({ ...l }));

    // ── Simulation ──
    const simulation = d3.forceSimulation<NetworkNode, NetworkLink>(nodes)
      .force('link', d3.forceLink<NetworkNode, NetworkLink>(links)
        .id(d => d.id)
        .distance(100)
        .strength(0.5))
      .force('charge', d3.forceManyBody().strength(-400))
      .force('center', d3.forceCenter(width / 2, height / 2))
      .force('collision', d3.forceCollide(NODE_SIZE * 1.5))
      .force('x', d3.forceX(width / 2).strength(0.05))
      .force('y', d3.forceY(height / 2).strength(0.05));

    simulationRef.current = simulation;

    // ── Draw links ──
    const linkGroup = root.append('g').attr('class', 'links');
    const linkElements = linkGroup
      .selectAll<SVGLineElement, NetworkLink>('line')
      .data(links)
      .join('line')
      .attr('stroke', d => {
        if (d.status === 'active') return '#334155';
        if (d.status === 'warning') return '#f59e0b';
        return '#1e293b';
      })
      .attr('stroke-width', d => d.status === 'active' ? 1.5 : 1)
      .attr('stroke-dasharray', d => d.status === 'inactive' ? '4,4' : 'none')
      .style('cursor', 'pointer')
      .on('click', (event, d) => {
        event.stopPropagation();
        onLinkClick?.(d);
      });

    // ── Draw VLAN cluster backgrounds ──
    const vlansInUse = [...new Set(nodes.map(n => n.vlan))];
    const vlanGroups = d3.group(nodes, d => d.vlan);

    const clusterGroup = root.append('g').attr('class', 'clusters');

    vlanGroups.forEach(([vlan, vlanNodes]) => {
      if (vlanNodes.length < 2) return; // Single-node clusters don't need background

      const color = VLAN_COLORS[vlan] ?? '#9ca3af';
      const padding = VLAN_CLUSTER_PADDING;

      // Compute bounding box for this VLAN's nodes
      const xs = vlanNodes.map(n => n.x ?? 0);
      const ys = vlanNodes.map(n => n.y ?? 0);
      const minX = Math.min(...xs) - NODE_SIZE - padding;
      const minY = Math.min(...ys) - NODE_SIZE - padding;
      const maxX = Math.max(...xs) + NODE_SIZE + padding;
      const maxY = Math.max(...ys) + NODE_SIZE + padding;

      clusterGroup.append('rect')
        .attr('x', minX)
        .attr('y', minY)
        .attr('width', maxX - minX)
        .attr('height', maxY - minY)
        .attr('rx', 12)
        .attr('fill', color)
        .attr('fill-opacity', 0.06)
        .attr('stroke', color)
        .attr('stroke-width', 1)
        .attr('stroke-opacity', 0.2)
        .attr('stroke-dasharray', '4,4');

      clusterGroup.append('text')
        .attr('x', minX + 8)
        .attr('y', minY + 14)
        .attr('fill', color)
        .attr('fill-opacity', 0.6)
        .attr('font-size', 9)
        .attr('font-family', 'ui-monospace, monospace')
        .attr('text-transform', 'uppercase')
        .attr('letter-spacing', '0.08em')
        .text(`VLAN ${vlan}`);
    });

    // ── Draw nodes ──
    const nodeGroup = root.append('g').attr('class', 'nodes');

    const nodeElements = nodeGroup
      .selectAll<SVGGElement, NetworkNode>('g.node')
      .data(nodes, d => d.id)
      .join('g')
      .attr('class', 'node')
      .style('cursor', 'pointer')
      .call(
        d3.drag<SVGGElement, NetworkNode>()
          .on('start', (event, d) => {
            if (!event.active) simulation.alphaTarget(0.3).restart();
            d.fx = d.x;
            d.fy = d.y;
          })
          .on('drag', (event, d) => {
            d.fx = event.x;
            d.fy = event.y;
          })
          .on('end', (event, d) => {
            if (!event.active) simulation.alphaTarget(0);
            d.fx = null;
            d.fy = null;
          })
      )
      .on('click', (event, d) => {
        event.stopPropagation();
        setSelectedNode(d);
        onNodeClick?.(d);
      })
      .on('dblclick', (event, d) => {
        event.stopPropagation();
        setSelectedNode(null);
      });

    // Node background circle (colored by VLAN)
    nodeElements.append('circle')
      .attr('r', NODE_SIZE)
      .attr('fill', d => VLAN_COLORS[d.vlan] ?? '#475569')
      .attr('fill-opacity', 0.15)
      .attr('stroke', d => VLAN_COLORS[d.vlan] ?? '#475569')
      .attr('stroke-width', 2)
      .attr('stroke-opacity', 0.6);

    // Status ring
    nodeElements.append('circle')
      .attr('r', NODE_SIZE + 4)
      .attr('fill', 'none')
      .attr('stroke', d => STATUS_COLORS[d.status])
      .attr('stroke-width', 2)
      .attr('stroke-opacity', d => d.status === 'offline' ? 0.3 : 0.8)
      .attr('filter', d => d.status === 'warning' ? 'url(#warning-glow)' : 'none');

    // Online glow
    nodeElements.filter(d => d.status === 'online')
      .attr('filter', 'url(#glow)');

    // Icon text
    nodeElements.append('text')
      .attr('text-anchor', 'middle')
      .attr('dominant-baseline', 'central')
      .attr('font-size', 16)
      .text(d => NODE_ICONS[d.type] ?? '\u{1F4E6}'); // 📦 default

    // Label (below node)
    nodeElements.append('text')
      .attr('text-anchor', 'middle')
      .attr('y', NODE_SIZE + 14)
      .attr('fill', '#e2e8f0')
      .attr('font-size', 10)
      .attr('font-family', 'ui-monospace, monospace')
      .attr('pointer-events', 'none')
      .text(d => d.label.length > 12 ? d.label.slice(0, 11) + '…' : d.label);

    // IP label (below label)
    nodeElements.append('text')
      .attr('text-anchor', 'middle')
      .attr('y', NODE_SIZE + 26)
      .attr('fill', '#64748b')
      .attr('font-size', 8)
      .attr('font-family', 'ui-monospace, monospace')
      .attr('pointer-events', 'none')
      .text(d => d.ip ?? '');

    // Status dot
    nodeElements.append('circle')
      .attr('cx', NODE_SIZE - 4)
      .attr('cy', -NODE_SIZE + 4)
      .attr('r', 5)
      .attr('fill', d => STATUS_COLORS[d.status])
      .attr('stroke', '#0f172a')
      .attr('stroke-width', 1.5);

    // ── Simulation tick ──
    simulation.on('tick', () => {
      linkElements
        .attr('x1', d => (d.source as NetworkNode).x ?? 0)
        .attr('y1', d => (d.source as NetworkNode).y ?? 0)
        .attr('x2', d => (d.target as NetworkNode).x ?? 0)
        .attr('y2', d => (d.target as NetworkNode).y ?? 0);

      nodeElements.attr('transform', d => `translate(${d.x ?? 0},${d.y ?? 0})`);
    });

    return () => {
      simulation.stop();
    };
  }, [topology, width, height, onNodeClick, onLinkClick]);

  // ── WebSocket live updates (optional) ─────────────────────
  useEffect(() => {
    if (!wsUrl) return;

    const ws = new WebSocket(wsUrl);

    ws.onmessage = event => {
      try {
        const update: { nodeId: string; status: NodeStatus } = JSON.parse(event.data);
        // In a real implementation, update topology data here via state
        // This would trigger a re-render of the D3 simulation
        console.log('Status update:', update);
      } catch {
        // Ignore malformed messages
      }
    };

    ws.onerror = () => {
      console.warn('WebSocket connection error — falling back to static data');
    };

    return () => ws.close();
  }, [wsUrl]);

  const vlans = useMemo(() => [...new Set(topology.nodes.map(n => n.vlan))], [topology]);

  return (
    <div
      ref={containerRef}
      style={{
        position: 'relative',
        width: '100%',
        height: '100%',
        minHeight: 400,
        background: 'linear-gradient(135deg, #0a0f1a 0%, #0f172a 100%)',
        borderRadius: 8,
        overflow: 'hidden',
      }}
    >
      {/* SVG Canvas */}
      <svg
        ref={svgRef}
        width={width}
        height={height}
        style={{ display: 'block' }}
      />

      {/* Detail Panel */}
      <DetailPanel
        node={selectedNode}
        links={topology.links}
        allNodes={topology.nodes}
        onClose={() => setSelectedNode(null)}
      />

      {/* VLAN Legend */}
      <VlanLegend vlans={vlans} />

      {/* Controls hint */}
      <div
        style={{
          position: 'absolute',
          top: 16,
          left: 16,
          color: '#475569',
          fontFamily: 'ui-monospace, monospace',
          fontSize: 10,
          pointerEvents: 'none',
        }}
      >
        <div>Drag to move</div>
        <div>Scroll to zoom</div>
        <div>Click node for details</div>
        <div>Double-click to deselect</div>
      </div>
    </div>
  );
};

// ─────────────────────────────────────────────────────────────
// Demo Data — 15 nodes, 7 VLANs, 19 links (your real infra)
// ─────────────────────────────────────────────────────────────

const DEMO_DATA: TopologyData = {
  nodes: [
    // Core Infrastructure
    { id: 'mikrotik-hq',  label: 'CCR1036-HQ',   type: 'router',   vlan: 100, ip: '10.10.101.1',   status: 'online',  metadata: { model: 'CCR1036-12G-4S+', firmware: 'v7.16.2' } },
    { id: 'dgx1',          label: 'DGX1-V100',     type: 'server',   vlan: 103, ip: '10.10.101.13',  status: 'online',  metadata: { gpu: '8x V100-32GB', cuda: '12.4', arch: 'x86_64' } },
    { id: 'annex4',        label: 'Annex-4-RTR',    type: 'router',   vlan: 106, ip: '10.10.101.1',   status: 'warning', metadata: { model: 'RB5009UG+S+', location: 'Annex-4' } },
    // Switches
    { id: 'sw-hq-01',      label: 'SW-HQ-01',       type: 'switch',   vlan: 100, ip: '10.10.101.2',   status: 'online' },
    { id: 'sw-hq-02',      label: 'SW-HQ-02',       type: 'switch',   vlan: 101, ip: '10.10.101.3',   status: 'online' },
    { id: 'sw-cctv',       label: 'SW-CCTV',        type: 'switch',   vlan: 40,  ip: '10.10.40.1',    status: 'online' },
    // Servers
    { id: 'nas-01',        label: 'NAS-01',         type: 'server',   vlan: 100, ip: '10.10.101.20',  status: 'online',  metadata: { storage: '48TB RAID-Z2' } },
    { id: 'prox-01',       label: 'Prox-01',        type: 'server',   vlan: 101, ip: '10.10.101.21',  status: 'online' },
    { id: 'homelab-01',    label: 'Homelab-01',    type: 'server',   vlan: 106, ip: '10.10.106.10',  status: 'warning', metadata: { role: 'vms + containers' } },
    // Laptops/Workstations
    { id: 'mbp16',          label: 'MBP16-Makati',   type: 'laptop',  vlan: 101, ip: '10.10.101.50',  status: 'online' },
    { id: 'sweep',          label: 'Sweep-Win',     type: 'laptop',  vlan: 101, ip: '10.10.101.51',  status: 'online',  metadata: { os: 'Win11-Pro', role: 'dev-machine' } },
    { id: 'devstation',      label: 'DevStation',    type: 'laptop',  vlan: 106, ip: '10.10.106.20',  status: 'offline' },
    // Guest/Camera VLANs
    { id: 'atlscreen',      label: 'ATL-Screen',    type: 'laptop',  vlan: 50,  ip: '10.10.50.10',  status: 'online' },
    { id: 'cam-front',      label: 'Cam-Front',     type: 'cloud',   vlan: 40,  ip: '10.10.40.10',  status: 'online',  metadata: { model: 'HIKVISION DS-2CD2043G2' } },
    { id: 'cam-back',       label: 'Cam-Back',      type: 'cloud',   vlan: 40,  ip: '10.10.40.11',  status: 'warning', metadata: { model: 'HIKVISION DS-2CD2043G2' } },
  ],
  links: [
    { id: 'l1',  source: 'mikrotik-hq', target: 'sw-hq-01',    bandwidth: '10G',  status: 'active' },
    { id: 'l2',  source: 'mikrotik-hq', target: 'sw-hq-02',    bandwidth: '10G',  status: 'active' },
    { id: 'l3',  source: 'mikrotik-hq', target: 'sw-cctv',     bandwidth: '1G',   status: 'active' },
    { id: 'l4',  source: 'sw-hq-01',    target: 'dgx1',         bandwidth: '10G',  status: 'active' },
    { id: 'l5',  source: 'sw-hq-01',    target: 'nas-01',        bandwidth: '10G',  status: 'active' },
    { id: 'l6',  source: 'sw-hq-01',    target: 'prox-01',       bandwidth: '1G',   status: 'active' },
    { id: 'l7',  source: 'sw-hq-01',    target: 'mbp16',         bandwidth: '1G',   status: 'active' },
    { id: 'l8',  source: 'sw-hq-01',    target: 'sweep',          bandwidth: '1G',   status: 'active' },
    { id: 'l9',  source: 'sw-hq-02',    target: 'homelab-01',   bandwidth: '1G',   status: 'active' },
    { id: 'l10', source: 'sw-hq-02',    target: 'devstation',    bandwidth: '1G',   status: 'inactive' },
    { id: 'l11', source: 'sw-hq-02',    target: 'annex4',        bandwidth: '1G',   status: 'warning' },
    { id: 'l12', source: 'sw-cctv',     target: 'cam-front',     bandwidth: '100M', status: 'active' },
    { id: 'l13', source: 'sw-cctv',     target: 'cam-back',      bandwidth: '100M', status: 'warning' },
    { id: 'l14', source: 'sw-hq-02',    target: 'atlscreen',    bandwidth: '1G',   status: 'active' },
    { id: 'l15', source: 'dgx1',         target: 'nas-01',        bandwidth: '10G',  status: 'active' },
    { id: 'l16', source: 'dgx1',         target: 'prox-01',       bandwidth: '10G',  status: 'active' },
    { id: 'l17', source: 'annex4',       target: 'homelab-01',  bandwidth: '1G',   status: 'active' },
    { id: 'l18', source: 'annex4',       target: 'devstation',  bandwidth: '1G',   status: 'inactive' },
    { id: 'l19', source: 'annex4',       target: 'sweep',        bandwidth: '1G',   status: 'warning' },
  ],
};

export default NetworkTopology;
