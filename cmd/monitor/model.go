package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// Panel enum
// ============================================================================

type Panel int

const (
	PanelMachines Panel = iota
	PanelDetails
	PanelEvents
	panelCount
)

// ============================================================================
// Tea messages
// ============================================================================

type pollResultMsg struct {
	machines []*MachineStatus
}

type tickMsg time.Time

// ============================================================================
// Model
// ============================================================================

type Model struct {
	machines    []*MachineStatus
	selected    int
	activePanel Panel
	events      []string
	width       int
	height      int
	polling     bool
	lastPoll    time.Time
	cfg         *Config
	pool        *sshPool
}

func initialModel(cfg *Config) Model {
	pool := newSSHPool(cfg.SSHTimeoutSeconds)
	machines := make([]*MachineStatus, len(cfg.Machines))
	for i, m := range cfg.Machines {
		machines[i] = &MachineStatus{
			Name: m.Name, Host: m.Host, Port: m.Port,
			User: m.User, Type: m.Type, Status: HealthUnknown,
		}
	}
	return Model{
		machines: machines,
		cfg:      cfg,
		pool:     pool,
		width:    120,
		height:   40,
	}
}

// ============================================================================
// Init
// ============================================================================

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		doPoll(m.cfg, m.pool),
		tickEvery(time.Duration(m.cfg.PollIntervalSeconds)*time.Second),
	)
}

// ============================================================================
// Update
// ============================================================================

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.pool.close()
			return m, tea.Quit
		case "tab":
			m.activePanel = (m.activePanel + 1) % panelCount
		case "j", "down":
			if m.selected < len(m.machines)-1 {
				m.selected++
			}
		case "k", "up":
			if m.selected > 0 {
				m.selected--
			}
		case "r":
			if !m.polling {
				m.polling = true
				return m, doPoll(m.cfg, m.pool)
			}
		}

	case tickMsg:
		if !m.polling {
			m.polling = true
			return m, tea.Batch(
				doPoll(m.cfg, m.pool),
				tickEvery(time.Duration(m.cfg.PollIntervalSeconds)*time.Second),
			)
		}
		return m, tickEvery(time.Duration(m.cfg.PollIntervalSeconds) * time.Second)

	case pollResultMsg:
		m.polling = false
		m.lastPoll = time.Now()
		m.machines = msg.machines
		for _, s := range msg.machines {
			if s != nil {
				m.events = appendEvent(m.events, buildEvent(s))
			}
		}
		// clamp selected
		if m.selected >= len(m.machines) {
			m.selected = len(m.machines) - 1
		}
	}

	return m, nil
}

// ============================================================================
// View
// ============================================================================

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	statusH := 1
	eventsH := 8
	mainH := m.height - statusH - eventsH
	if mainH < 6 {
		mainH = 6
	}

	leftW := 28
	rightW := m.width - leftW
	if rightW < 20 {
		rightW = 20
		leftW = m.width - rightW
	}

	var sel *MachineStatus
	if m.selected >= 0 && m.selected < len(m.machines) {
		sel = m.machines[m.selected]
	}

	leftPanel := renderMachinesPanel(m.machines, m.selected,
		m.activePanel == PanelMachines, leftW, mainH)
	rightPanel := renderDetailsPanel(sel,
		m.activePanel == PanelDetails, rightW, mainH)

	topRow := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)

	eventsPanel := renderEventsPanel(m.events,
		m.activePanel == PanelEvents, m.width, eventsH)

	statusBar := renderStatusBar(m.machines, m.lastPoll, m.polling, m.width)

	return lipgloss.JoinVertical(lipgloss.Left, topRow, eventsPanel, statusBar)
}

// ============================================================================
// Commands
// ============================================================================

func doPoll(cfg *Config, pool *sshPool) tea.Cmd {
	return func() tea.Msg {
		return pollResultMsg{machines: pollAll(cfg, pool)}
	}
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// ============================================================================
// Config loader
// ============================================================================

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 15
	}
	if cfg.SSHTimeoutSeconds <= 0 {
		cfg.SSHTimeoutSeconds = 10
	}
	return &cfg, nil
}
