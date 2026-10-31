package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"sandbox-cloud/internal/cloud"
)

var accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD"))
var muted = lipgloss.NewStyle().Foreground(lipgloss.Color("#8994A8"))
var green = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A"))
var amber = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68"))
var red = lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E"))
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type tickMsg time.Time
type pollMsg struct {
	items []cloud.Workspace
	err   error
}
type pollFunc func(context.Context) ([]cloud.Workspace, error)

type dashboard struct {
	ctx         context.Context
	fetch       pollFunc
	items       []cloud.Workspace
	frame       int
	width       int
	wait        bool
	preview     bool
	done        bool
	interrupted bool
	err         error
	started     time.Time
}

func tick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m dashboard) poll(delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-m.ctx.Done():
				return pollMsg{err: m.ctx.Err()}
			case <-timer.C:
			}
		}
		items, err := m.fetch(m.ctx)
		return pollMsg{items, err}
	}
}
func (m dashboard) Init() tea.Cmd { return tea.Batch(tick(), m.poll(0)) }
func (m dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" || msg.String() == "esc" {
			m.interrupted = true
			m.done = true
			return m, tea.Quit
		}
	case tickMsg:
		m.frame++
		return m, tick()
	case pollMsg:
		if msg.err != nil {
			m.err = msg.err
			m.done = true
			return m, tea.Quit
		}
		m.items = msg.items
		if m.wait && len(m.items) == 1 {
			switch m.items[0].State {
			case "ready":
				m.done = true
				return m, tea.Quit
			case "terminated", "expired", "terminating", "unavailable":
				m.err = fmt.Errorf("workspace is %s: %s", m.items[0].State, m.items[0].Reason)
				m.done = true
				return m, tea.Quit
			}
		}
		return m, m.poll(time.Second)
	}
	return m, nil
}
func stateStyle(s string) lipgloss.Style {
	switch s {
	case "ready":
		return green
	case "pending", "starting":
		return amber
	default:
		return red
	}
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, ansi.Strip(s))
}
func remaining(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return d.Round(time.Second).String()
}
func (m dashboard) View() string {
	width := m.width - 6
	if width < 30 {
		width = 30
	}
	if width > 94 {
		width = 94
	}
	var b strings.Builder
	title := "SANDBOX / CLOUD"
	if m.preview {
		title += "    UI PREVIEW · simulated data"
	}
	b.WriteString(accent.Bold(true).Render(title) + "\n")
	b.WriteString(muted.Render("Disposable workspaces · Kubernetes owns the lifecycle") + "\n\n")
	if len(m.items) == 0 {
		b.WriteString(muted.Render("No workspaces. Create one with sandbox create.") + "\n")
	}
	for _, w := range m.items {
		indicator := "●"
		if w.State == "pending" || w.State == "starting" {
			indicator = frames[m.frame%len(frames)]
		}
		b.WriteString(stateStyle(w.State).Render(indicator+" "+strings.ToUpper(w.State)) + "  " + lipgloss.NewStyle().Bold(true).Render(clean(w.ID)) + "\n")
		b.WriteString(muted.Render("  CPU "+w.CPU+" · Memory "+w.Memory+" · TTL "+remaining(w.ExpiresAt)) + "\n")
		if w.Node != "" {
			b.WriteString(muted.Render("  Node "+ansi.Truncate(clean(w.Node), width-8, "…")) + "\n")
		}
		if w.Reason != "" {
			b.WriteString(amber.Render("  "+ansi.Truncate(clean(w.Reason), width-4, "…")) + "\n")
		}
		if m.wait {
			steps := []string{"Workspace accepted", "Scheduled on a worker", "Runner ready"}
			reached := 0
			if w.Node != "" {
				reached = 1
			}
			if w.State == "ready" {
				reached = 2
			}
			for i, step := range steps {
				mark := muted.Render("○")
				if i <= reached {
					mark = green.Render("✓")
				} else if i == reached+1 && !m.done {
					mark = accent.Render(frames[m.frame%len(frames)])
				}
				b.WriteString("  " + mark + " " + step + "\n")
			}
		}
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(red.Render("Error: "+clean(m.err.Error())) + "\n")
	}
	if m.done && m.wait && m.err == nil && !m.interrupted && len(m.items) == 1 {
		b.WriteString(green.Render("Ready in "+time.Since(m.started).Round(100*time.Millisecond).String()) + "\n" + muted.Render("Next: sandbox exec "+m.items[0].ID+" -- go version"))
	} else {
		b.WriteString(muted.Render("q / esc to leave · workspaces continue running"))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#414868")).Padding(1, 2).Width(width).Render(b.String()) + "\n"
}
func show(ctx context.Context, out io.Writer, fetch pollFunc, wait, preview bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := dashboard{ctx: ctx, fetch: fetch, wait: wait, preview: preview, width: 90, started: time.Now()}
	result, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(out)).Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	final := result.(dashboard)
	if final.interrupted {
		return context.Canceled
	}
	return final.err
}
