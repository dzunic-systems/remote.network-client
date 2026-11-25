package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"remote.network/client/internal/agent"
)

// ttyAsker asks the permission prompt on the terminal: the tool, its
// server and its arguments, then Allow once / Always allow / Deny. With
// no TTY there is nobody to ask — construction fails and ask denies
// (the plan's rule).
type ttyAsker struct {
	in  *bufio.Reader
	out io.Writer
}

// newTTYAsker returns nil when stdin is not a terminal.
func newTTYAsker() *ttyAsker {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	return &ttyAsker{in: bufio.NewReader(os.Stdin), out: os.Stderr}
}

// Ask implements agent.Asker. EOF or an unreadable answer denies — a
// prompt nobody can answer must not hang the turn.
func (a *ttyAsker) Ask(ctx context.Context, req agent.AskRequest) (agent.Decision, error) {
	args := req.Arguments
	var pretty json.RawMessage
	if json.RawMessage(args) != nil && json.Unmarshal([]byte(args), &pretty) == nil {
		if b, err := json.MarshalIndent(pretty, "  ", "  "); err == nil {
			args = string(b)
		}
	}
	fmt.Fprintf(a.out, "\n🔧 tool request: %s (server %q)\n  arguments: %s\n", req.Name, req.Server, args)
	fmt.Fprintf(a.out, "  [1] allow once   [2] always allow this tool   [3] deny > ")
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		return agent.Deny, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "1", "y", "yes", "once", "allow":
		return agent.Once, nil
	case "2", "always":
		return agent.Always, nil
	default: // 3, d, n, no, deny, empty line
		return agent.Deny, nil
	}
}
