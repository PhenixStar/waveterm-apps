package main

import (
	"fmt"
	"strings"
	"time"
)

const maxEvents = 50

// ============================================================================
// Helpers
// ============================================================================

func formatUptime(secs int64) string {
	if secs <= 0 {
		return "--"
	}
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func formatBytes(b uint64) string {
	const gib = 1 << 30
	const mib = 1 << 20
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/mib)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func statusDot(s *MachineStatus) string {
	if s == nil {
		return styleMuted.Render("?")
	}
	switch s.Status {
	case HealthOK:
		return styleGreen.Render("\u25cf")
	case HealthWarning:
		return styleYellow.Render("\u25cf")
	case HealthCritical:
		return styleRed.Render("\u25cf")
	default:
		return styleMuted.Render("\u25cb")
	}
}

func typeTag(t MachineType) string {
	switch t {
	case TypeLinux:
		return styleMuted.Render("[lnx]")
	case TypeWindows:
		return styleMuted.Render("[win]")
	case TypeMikroTik:
		return styleMuted.Render("[mtk]")
	default:
		return styleMuted.Render("[???]")
	}
}

// ============================================================================
// Machine list panel
// ============================================================================

func renderMachinesPanel(machines []*MachineStatus, selected int, active bool, width, height int) string {
	innerW := width - 2
	if innerW < 1 {
		innerW = 1
	}

	title := stylePanelTitle.Render("Machines")

	var rows []string
	for i, s := range machines {
		if s == nil {
			continue
		}
		dot := statusDot(s)
		tag := typeTag(s.Type)

		name := s.Name
		maxName := innerW - 12
		if maxName < 4 {
			maxName = 4
		}
		if len(name) > maxName {
			name = name[:maxName-1] + "\u2026"
		}

		cpuStr := ""
		if s.Status != HealthUnknown {
			cpuStr = fmt.Sprintf("%4.0f%%", s.CPU)
		} else {
			cpuStr = "  -- "
		}

		line := fmt.Sprintf(" %s %-*s %s %s", dot, maxName, name, tag, styleMuted.Render(cpuStr))

		if i == selected {
			// pad to full width for highlight
			plain := fmt.Sprintf("  %-*s      %s", maxName, name, cpuStr)
			if len(plain) < innerW {
				plain += strings.Repeat(" ", innerW-len(plain))
			}
			line = fmt.Sprintf(" %s %-*s %s %s", dot, maxName, styleSelected.Render(name), tag, styleCyan.Render(cpuStr))
		}

		rows = append(rows, line)
	}

	// pad to fill height
	contentLines := len(rows) + 2 // title + blank
	for contentLines < height-2 {
		rows = append(rows, "")
		contentLines++
	}

	content := title + "\n" + strings.Join(rows, "\n")

	style := stylePanelBase
	if active {
		style = stylePanelActive
	}
	return style.Width(width).Height(height).Render(content)
}

// ============================================================================
// Details panel
// ============================================================================

func renderDetailsPanel(s *MachineStatus, active bool, width, height int) string {
	innerW := width - 4
	if innerW < 10 {
		innerW = 10
	}
	barW := innerW - 20
	if barW < 6 {
		barW = 6
	}

	title := stylePanelTitle.Render("Details")

	var lines []string

	if s == nil {
		lines = append(lines, "", styleMuted.Render("  No machine selected"))
	} else if s.Status == HealthUnknown && s.LastError != "" {
		lines = append(lines,
			"",
			styleBold.Render("  "+s.Name),
			"  "+styleMuted.Render(s.Host),
			"",
			styleRed.Render("  UNREACHABLE"),
			"  "+styleMuted.Render(truncate(s.LastError, innerW-2)),
			"",
			"  Last seen: "+styleMuted.Render(s.LastUpdate.Format("15:04:05")),
		)
	} else {
		lines = append(lines,
			"",
			"  "+styleBold.Render(s.Name)+"  "+styleMuted.Render(s.Host),
			"",
		)

		metricLine := func(label string, pct float64, used, total uint64, suffix string) string {
			bar := renderBar(pct, barW)
			col := metricColor(pct)
			pctStr := col.Render(fmt.Sprintf("%5.1f%%", pct))
			labelStr := styleMuted.Render(fmt.Sprintf("  %-5s", label))
			detail := ""
			if total > 0 {
				detail = styleMuted.Render(fmt.Sprintf("  %s/%s", formatBytes(used), formatBytes(total)))
			}
			return fmt.Sprintf("%s %s %s%s%s", labelStr, bar, pctStr, suffix, detail)
		}

		lines = append(lines, metricLine("CPU", s.CPU, 0, 0, ""))
		lines = append(lines, metricLine("MEM", s.MemoryPercent, s.MemoryUsed, s.MemoryTotal, ""))
		lines = append(lines, metricLine("DISK", s.DiskPercent, s.DiskUsed, s.DiskTotal, ""))

		if len(s.GPUs) > 0 {
			lines = append(lines, "")
			for i, g := range s.GPUs {
				if i >= 4 {
					lines = append(lines, styleMuted.Render(fmt.Sprintf("  ... +%d more GPUs", len(s.GPUs)-4)))
					break
				}
				gpuLabel := fmt.Sprintf("GPU%d", g.Index)
				lines = append(lines, metricLine(gpuLabel, g.UtilPercent, g.MemUsed, g.MemTotal, ""))
				vramLine := metricLine("VRAM", g.MemPercent, g.MemUsed, g.MemTotal, "")
				lines = append(lines, vramLine)
			}
		}

		lines = append(lines, "")

		if s.Docker != nil {
			dockerStr := fmt.Sprintf("  Docker: %s running / %s total",
				styleGreen.Render(fmt.Sprintf("%d", s.Docker.Running)),
				styleWhite.Render(fmt.Sprintf("%d", s.Docker.Total)))
			lines = append(lines, dockerStr)
		}

		if s.MikroTik != nil {
			lines = append(lines,
				fmt.Sprintf("  WiFi clients: %s", styleCyan.Render(fmt.Sprintf("%d", s.MikroTik.WiFiClients))),
				fmt.Sprintf("  ETH Rx/Tx: %s / %s",
					styleGreen.Render(formatBytes(s.MikroTik.ETHRx)),
					styleCyan.Render(formatBytes(s.MikroTik.ETHTx))),
			)
		}

		lines = append(lines,
			fmt.Sprintf("  Uptime: %s", styleCyan.Render(formatUptime(s.UptimeSeconds))),
			"  "+styleMuted.Render("Polled: "+s.LastUpdate.Format("15:04:05")),
		)
	}

	for len(lines) < height-4 {
		lines = append(lines, "")
	}

	content := title + "\n" + strings.Join(lines, "\n")
	style := stylePanelBase
	if active {
		style = stylePanelActive
	}
	return style.Width(width).Height(height).Render(content)
}

// ============================================================================
// Events panel
// ============================================================================

func renderEventsPanel(events []string, active bool, width, height int) string {
	title := stylePanelTitle.Render("Events")

	innerH := height - 4
	if innerH < 1 {
		innerH = 1
	}

	// show most recent events that fit
	visible := events
	if len(visible) > innerH {
		visible = visible[len(visible)-innerH:]
	}

	var lines []string
	for _, e := range visible {
		lines = append(lines, "  "+truncate(e, width-6))
	}
	for len(lines) < innerH {
		lines = append(lines, "")
	}

	content := title + "\n" + strings.Join(lines, "\n")
	style := stylePanelBase
	if active {
		style = stylePanelActive
	}
	return style.Width(width).Height(height).Render(content)
}

// ============================================================================
// Status bar
// ============================================================================

func renderStatusBar(machines []*MachineStatus, lastPoll time.Time, polling bool, width int) string {
	online, offline := 0, 0
	for _, m := range machines {
		if m == nil || m.Status == HealthUnknown {
			offline++
		} else {
			online++
		}
	}

	left := fmt.Sprintf(" %s online  %s offline",
		styleGreen.Render(fmt.Sprintf("%d", online)),
		styleRed.Render(fmt.Sprintf("%d", offline)),
	)

	center := ""
	if polling {
		center = styleYellow.Render(" polling...")
	} else if !lastPoll.IsZero() {
		center = styleMuted.Render("last poll: " + lastPoll.Format("15:04:05"))
	}

	right := styleMuted.Render("[j/k] select  [Tab] panel  [r] refresh  [q] quit ")

	// naive layout: left + spaces + center + spaces + right
	leftLen := visibleLen(left)
	centerLen := visibleLen(center)
	rightLen := visibleLen(right)
	gap := width - leftLen - centerLen - rightLen
	if gap < 2 {
		gap = 2
	}
	leftPad := gap / 2
	rightPad := gap - leftPad

	bar := left + strings.Repeat(" ", leftPad) + center + strings.Repeat(" ", rightPad) + right
	return styleStatusBar.Width(width).Render(bar)
}

// visibleLen estimates printable character count (strip ANSI escape codes).
func visibleLen(s string) int {
	inEsc := false
	count := 0
	for _, ch := range s {
		if ch == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if ch == 'm' {
				inEsc = false
			}
			continue
		}
		count++
	}
	return count
}

// ============================================================================
// Event builder
// ============================================================================

func buildEvent(s *MachineStatus) string {
	ts := time.Now().Format("15:04:05")
	if s.Status == HealthUnknown || s.LastError != "" {
		return styleMuted.Render(ts) + " " + styleRed.Render(s.Name) + " " + styleMuted.Render("unreachable")
	}
	metrics := fmt.Sprintf("cpu=%.0f%% mem=%.0f%%", s.CPU, s.MemoryPercent)
	return styleMuted.Render(ts) + " " + styleCyan.Render(s.Name) + " " +
		styleGreen.Render("ok") + " " + styleMuted.Render("("+metrics+")")
}

func appendEvent(events []string, e string) []string {
	events = append(events, e)
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 1 {
		return ""
	}
	return s[:max-1] + "\u2026"
}
