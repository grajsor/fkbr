package tui

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type idleModel struct{}

func (idleModel) Init() tea.Cmd                       { return nil }
func (idleModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return idleModel{}, nil }
func (idleModel) View() string                        { return "" }

// A SIGTERM ends the TUI: main cancels its context on it. bubbletea's own
// handler took the same signal and sent QuitMsg to an event loop the cancel
// had already ended; its shutdown then waited for that send for good (a fuzzed
// TUI lived on for 18 h after its SIGTERM; 10 h run 2026-11-01).
func TestQuitsOnSIGTERM(t *testing.T) {
	sig := make(chan os.Signal, 16) // what main's NotifyContext does: SIGTERM must not kill the test
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	for i := range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		popts := append(programOptions(ctx, Options{NoMouse: true}), tea.WithInput(nil), tea.WithOutput(io.Discard))
		p := tea.NewProgram(idleModel{}, popts...)
		done := make(chan error, 1)
		go func() { _, err := p.Run(); done <- err }()
		time.Sleep(20 * time.Millisecond) // running, handlers set up
		go cancel()
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the program did not end after SIGTERM and its context's cancel", i)
		}
		cancel()
	}
}
