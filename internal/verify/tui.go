/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/egoavara/route-prism/internal/preflight"
)

// ---------- styles ----------

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7C3AED"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))
	currentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#10B981"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B"))
	stepDoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981"))
	stepFailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444"))
	stepRunStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#3B82F6"))
	headStyle     = lipgloss.NewStyle().Bold(true)
)

// ---------- messages ----------

type resultMsg Result
type doneListening struct{}

// ---------- model ----------

type uiState int

const (
	statePicking uiState = iota
	stateRunning
	stateDone
)

// Model is the top-level bubbletea model for the verify TUI.
type Model struct {
	state    uiState
	contexts []Context
	cursor   int

	chosen     *Context
	steps      []Step
	result     *Result
	mesh       preflight.MeshInfo
	spinner    spinner.Model
	cancelFunc context.CancelFunc

	opts Options
	err  error
	w, h int
}

// NewModel constructs the picker model.
func NewModel(contexts []Context, opts Options) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	// Default cursor to the current context if any.
	cursor := 0
	for i, c := range contexts {
		if c.IsCurrent {
			cursor = i
			break
		}
	}

	return Model{
		state:    statePicking,
		contexts: contexts,
		cursor:   cursor,
		spinner:  sp,
		opts:     opts,
	}
}

func (m Model) Init() tea.Cmd {
	return m.spinner.Tick
}

// ---------- commands ----------

func (m *Model) startVerification() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	opts := m.opts
	opts.ContextName = m.chosen.Name
	stream := Run(ctx, opts)
	return waitStep(stream)
}

func waitStep(stream Stream) tea.Cmd {
	return func() tea.Msg {
		select {
		case s, ok := <-stream.Steps:
			if !ok {
				// Steps closed; collect final result.
				res, ok := <-stream.Result
				if !ok {
					return doneListening{}
				}
				return resultMsg(res)
			}
			// Issue another wait via a follow-up command.
			return stepEvent{step: s, stream: stream}
		case res, ok := <-stream.Result:
			if !ok {
				return doneListening{}
			}
			return resultMsg(res)
		}
	}
}

type stepEvent struct {
	step   Step
	stream Stream
}

// ---------- update ----------

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case stepEvent:
		m.steps = append(m.steps, msg.step)
		return m, waitStep(msg.stream)

	case resultMsg:
		m.state = stateDone
		r := Result(msg)
		m.result = &r
		m.mesh = r.Report.Mesh
		return m, nil

	case doneListening:
		m.state = stateDone
		return m, nil
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.state {
	case statePicking:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.contexts)-1 {
				m.cursor++
			}
		case "enter":
			c := m.contexts[m.cursor]
			m.chosen = &c
			m.state = stateRunning
			return m, m.startVerification()
		}

	case stateRunning:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.cancelFunc != nil {
				m.cancelFunc()
			}
			return m, tea.Quit
		}

	case stateDone:
		switch msg.String() {
		case "ctrl+c", "q", "esc", "enter":
			return m, tea.Quit
		}
	}
	return m, nil
}

// ---------- view ----------

func (m Model) View() string {
	switch m.state {
	case statePicking:
		return m.viewPick()
	case stateRunning:
		return m.viewRunning()
	case stateDone:
		return m.viewDone()
	}
	return ""
}

func (m Model) viewPick() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("route-prism verify"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Select a kubeconfig context to verify Gateway API GAMMA support."))
	b.WriteString("\n\n")

	for i, c := range m.contexts {
		marker := "  "
		if i == m.cursor {
			marker = cursorStyle.Render("▸ ")
		}
		name := c.Name
		if c.IsCurrent {
			name = currentStyle.Render(name + " (current)")
		}
		extras := []string{}
		if c.Cluster != "" {
			extras = append(extras, "cluster="+c.Cluster)
		}
		if c.Namespace != "" {
			extras = append(extras, "ns="+c.Namespace)
		}
		extra := ""
		if len(extras) > 0 {
			extra = "  " + dimStyle.Render(strings.Join(extras, " "))
		}
		fmt.Fprintf(&b, "%s%s%s\n", marker, name, extra)
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("↑/↓ move · enter select · q quit"))
	return b.String()
}

func (m Model) viewRunning() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("route-prism verify"))
	b.WriteString("  ")
	if m.chosen != nil {
		b.WriteString(dimStyle.Render("→ " + m.chosen.Name))
	}
	b.WriteString("\n\n")

	for _, s := range m.steps {
		b.WriteString(renderStep(s, m.spinner.View()))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("ctrl+c to abort"))
	return b.String()
}

func (m Model) viewDone() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("route-prism verify — result"))
	b.WriteString("\n\n")

	for _, s := range m.steps {
		b.WriteString(renderStep(s, ""))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if m.result != nil {
		b.WriteString(headStyle.Render("Mesh"))
		b.WriteString(": ")
		b.WriteString(m.mesh.Summary())
		b.WriteString("\n")
		b.WriteString(headStyle.Render("Outcome"))
		b.WriteString(": ")
		b.WriteString(renderOutcome(m.result.Report))
		b.WriteString("\n\n")

		if len(m.result.Cases) > 0 {
			b.WriteString(headStyle.Render("Traffic cases"))
			b.WriteString("\n")
			for _, tc := range m.result.Cases {
				icon := stepDoneStyle.Render("✓")
				if !tc.OK {
					icon = stepFailStyle.Render("✗")
				}
				detail := fmt.Sprintf("expected=%s got=%s", tc.Expected, tc.GotVariant)
				if tc.Err != nil {
					detail += "  " + tc.Err.Error()
				}
				fmt.Fprintf(&b, "  %s %s  %s\n", icon, tc.Name, dimStyle.Render(detail))
			}
			b.WriteString("\n")
		}

		recs := Recommend(m.result.Report)
		if len(recs) > 0 {
			b.WriteString(headStyle.Render("Recommendations"))
			b.WriteString("\n")
			for _, r := range recs {
				b.WriteString("  • ")
				b.WriteString(r)
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("press enter to exit"))
	return b.String()
}

func renderStep(s Step, spin string) string {
	icon := stepRunStyle.Render(spin)
	if s.OK != nil {
		if *s.OK {
			icon = stepDoneStyle.Render("✓")
		} else {
			icon = stepFailStyle.Render("✗")
		}
	}
	if icon == "" {
		icon = "·"
	}
	line := fmt.Sprintf("%s %s", icon, s.Title)
	if s.Detail != "" {
		line += "  " + dimStyle.Render(s.Detail)
	}
	return line
}

func renderOutcome(r preflight.ProbeReport) string {
	switch r.Outcome {
	case preflight.OutcomeAccepted:
		return stepDoneStyle.Render("Accepted") + dimStyle.Render(fmt.Sprintf(" by %s", r.ControllerName))
	case preflight.OutcomeRejected:
		return stepFailStyle.Render("Rejected") + dimStyle.Render(fmt.Sprintf(" by %s — %s", r.ControllerName, r.Reason))
	case preflight.OutcomeNoController:
		return stepFailStyle.Render("No controller picked it up")
	case preflight.OutcomeCRDMissing:
		return stepFailStyle.Render("HTTPRoute CRD missing")
	case preflight.OutcomeProbeError:
		msg := ""
		if r.Err != nil {
			msg = " — " + r.Err.Error()
		}
		return stepFailStyle.Render("Probe error") + dimStyle.Render(msg)
	}
	return string(r.Outcome)
}

// RunPlain is the non-TUI fallback used when stdout is not a TTY or
// --no-tui is passed. It streams steps as plain log lines and prints a
// final report on stderr/stdout suitable for CI capture.
func RunPlain(ctx context.Context, opts Options) (preflight.ProbeReport, []TrafficCase, error) {
	stream := Run(ctx, opts)
	for s := range stream.Steps {
		state := "…"
		if s.OK != nil {
			if *s.OK {
				state = "ok"
			} else {
				state = "FAIL"
			}
		}
		line := fmt.Sprintf("[%s] %s", state, s.Title)
		if s.Detail != "" {
			line += " — " + s.Detail
		}
		fmt.Println(line)
	}
	res := <-stream.Result
	return res.Report, res.Cases, res.Err
}
