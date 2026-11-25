// Package cli is remote-client's command tree.
//
// `serve` is the local OpenAI-compatible endpoint (direct path when one
// exists, the router otherwise); `chat` is the same path one-shot;
// `doctor` checks reachability; `login` stores the API key.
package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

// Version is stamped at build time (-ldflags -X …/cli.Version=…).
var Version = "dev"

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "remote-client",
		Short: "Use the remote.network inference network from this machine",
		Long: "remote-client runs a local OpenAI-compatible endpoint for the remote.network\n" +
			"inference network. Requests go straight to the serving node over QUIC when a\n" +
			"direct path exists, and through a router otherwise.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("remote-client {{.Version}}\n")
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "remote-client %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		},
	})
	root.AddCommand(newLoginCommand())
	root.AddCommand(newServeCommand())
	root.AddCommand(newChatCommand())
	root.AddCommand(newDoctorCommand())
	return root
}

// Execute runs the command tree and exits non-zero on error.
func Execute() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-client:", err)
		os.Exit(1)
	}
}
