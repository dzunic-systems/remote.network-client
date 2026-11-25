package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"remote.network/client/internal/agent"
)

// chatPrinter renders the agent loop's events for the terminal: the
// answer streams to stdout as it generates; the model's thinking streams
// to stderr (dimmed on a terminal), so a thinking model shows it is
// working instead of looking hung; tool activity is one line per call.
// Tool arguments and results print as text only.
type chatPrinter struct {
	out, errw io.Writer
	dim       bool
	thinking  bool // inside a thinking run on stderr
	wroteText bool
}

func newChatPrinter(cmd *cobra.Command) *chatPrinter {
	errw := cmd.ErrOrStderr()
	dim := false
	if f, ok := errw.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			dim = true
		}
	}
	return &chatPrinter{out: cmd.OutOrStdout(), errw: errw, dim: dim}
}

func (p *chatPrinter) endThinking() {
	if p.thinking {
		if p.dim {
			fmt.Fprint(p.errw, "\x1b[0m")
		}
		fmt.Fprintln(p.errw)
		p.thinking = false
	}
}

func (p *chatPrinter) event(ev agent.Event) error {
	switch ev.Type {
	case "delta":
		if ev.Reasoning != "" {
			if !p.thinking {
				p.thinking = true
				if p.dim {
					fmt.Fprint(p.errw, "\x1b[2m")
				}
				fmt.Fprint(p.errw, "thinking: ")
			}
			fmt.Fprint(p.errw, ev.Reasoning)
		}
		if ev.Content != "" {
			p.endThinking()
			fmt.Fprint(p.out, ev.Content)
			p.wroteText = true
		}
	case "tool_call":
		p.endThinking()
		fmt.Fprintf(p.out, "\n🔧 %s", ev.Name)
	case "tool_result":
		status := "ok"
		if !ev.OK && ev.Text == "denied by the user" {
			status = "denied"
		} else if !ev.OK {
			status = "failed"
		}
		fmt.Fprintf(p.out, " → %s (%d ms, %s)\n", status, ev.MS, humanBytes(ev.Bytes))
	case "disclosure":
		p.endThinking()
		fmt.Fprintf(p.errw, "note: %s\n", ev.Text)
	case "error":
		p.endThinking()
		fmt.Fprintf(p.errw, "%s\n", ev.Message)
	case "done":
		p.endThinking()
		if ev.Finish == "length" {
			fmt.Fprintf(p.errw, "\n(the answer stopped at the output token limit)\n")
		}
	}
	return nil
}

// finish ends the output cleanly (the color reset, a final newline).
func (p *chatPrinter) finish() {
	p.endThinking()
	if p.wroteText {
		fmt.Fprintln(p.out)
	}
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// jsonRawString marshals s as a JSON string (message Content rides as a
// JSON string on the wire, not bare text).
func jsonRawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
