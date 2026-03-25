package main

import "github.com/charmbracelet/lipgloss"

var (
	colorBorder       = lipgloss.Color("#232345")
	colorActiveBorder = lipgloss.Color("#06b6d4")
	colorBg           = lipgloss.Color("#0d0d1a")
	colorStatusBg     = lipgloss.Color("#111124")
	colorMuted        = lipgloss.Color("#5a6077")
	colorCyan         = lipgloss.Color("#06b6d4")
	colorGreen        = lipgloss.Color("#10b981")
	colorYellow       = lipgloss.Color("#eab308")
	colorRed          = lipgloss.Color("#ef4444")
	colorWhite        = lipgloss.Color("#e2e8f0")
	colorDim          = lipgloss.Color("#3d4466")

	stylePanelBase = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder)

	stylePanelActive = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorActiveBorder)

	styleStatusBar = lipgloss.NewStyle().
			Background(colorStatusBg).
			Foreground(colorMuted).
			Padding(0, 1)

	styleMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	styleCyan   = lipgloss.NewStyle().Foreground(colorCyan)
	styleGreen  = lipgloss.NewStyle().Foreground(colorGreen)
	styleYellow = lipgloss.NewStyle().Foreground(colorYellow)
	styleRed    = lipgloss.NewStyle().Foreground(colorRed)
	styleWhite  = lipgloss.NewStyle().Foreground(colorWhite)
	styleBold   = lipgloss.NewStyle().Foreground(colorWhite).Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(colorDim)

	styleSelected = lipgloss.NewStyle().
			Background(lipgloss.Color("#1a1a3a")).
			Foreground(colorCyan).
			Bold(true)

	stylePanelTitle = lipgloss.NewStyle().
			Foreground(colorCyan).
			Bold(true).
			Padding(0, 1)
)

// metricColor returns a style based on percentage thresholds.
func metricColor(pct float64) lipgloss.Style {
	switch {
	case pct >= 80:
		return styleRed
	case pct >= 50:
		return styleYellow
	default:
		return styleGreen
	}
}

// renderBar renders a filled/empty bar of given width for a percentage.
func renderBar(pct float64, width int) string {
	if width <= 0 {
		width = 10
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	bar := ""
	for i := 0; i < filled; i++ {
		bar += "\u2588"
	}
	for i := filled; i < width; i++ {
		bar += "\u2591"
	}
	col := metricColor(pct)
	return col.Render(bar)
}
