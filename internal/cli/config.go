package cli

import (
	"fmt"
	"net"
	"strconv"

	"github.com/spf13/cobra"

	"remote.network/client/internal/config"
)

// newConfigCommand is `remote-client config`: show / set / unset — every
// setting in one place, each printed with the source it resolved from.
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change the settings (the config file)",
		Long: "config shows every setting with its effective value and its source\n" +
			"(flag > environment > config file > default), and sets or clears the\n" +
			"config file's values. Secrets are refused here: store the API key with\n" +
			"`login`, pass the local token with --local-token.",
	}
	cmd.AddCommand(newConfigShowCommand(), newConfigSetCommand(), newConfigUnsetCommand())
	return cmd
}

func newConfigShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print every setting, its effective value and its source",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := config.Resolve(config.Flags{})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			row := func(name string, v config.Value[string]) {
				fmt.Fprintf(out, "%-14s %s\n", name, orUnset(v.Value))
				fmt.Fprintf(out, "%-14s   %s\n", "", v.Source)
			}
			rowBool := func(name string, v config.Value[bool]) {
				fmt.Fprintf(out, "%-14s %s\n", name, onOff(v.Value))
				fmt.Fprintf(out, "%-14s   %s\n", "", v.Source)
			}
			row("home", s.Home)
			row("router", s.Router)
			row("router_alt", s.RouterAlt)
			fmt.Fprintf(out, "%-14s %s\n", "listen", s.Listen.Value)
			fmt.Fprintf(out, "%-14s   %s\n", "", s.Listen.Source)
			fmt.Fprintf(out, "%-14s %d\n", "port", s.Port.Value)
			fmt.Fprintf(out, "%-14s   %s\n", "", s.Port.Source)
			// Secrets: presence and source, never the value.
			fmt.Fprintf(out, "%-14s %s\n", "api_key", secretState(s.APIKey))
			fmt.Fprintf(out, "%-14s   %s\n", "", s.APIKey.Source)
			row("api_key_file", s.APIKeyFile)
			fmt.Fprintf(out, "%-14s %s\n", "local_token", secretState(s.LocalToken))
			fmt.Fprintf(out, "%-14s   %s\n", "", s.LocalToken.Source)
			rowBool("direct", s.Direct)
			row("log_level", s.LogLevel)
			rowBool("ui", s.UI)
			row("default_model", s.DefaultModel)
			rowBool("web_search", s.WebSearch)
			row("mcp_config", s.MCPConfig)
			rowBool("tools", s.Tools)
			return nil
		},
	}
}

func newConfigSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write one setting to the config file (validated as at startup)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg, err = config.SetFileKey(cfg, key, value); err != nil {
				return err
			}
			// The port/listen pair is validated as a pair (a startup error
			// later would be too late to be friendly).
			if err := checkPortListen(cfg); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			path, err := config.Path()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s written to %s\n", key, path)
			return nil
		},
	}
}

func newConfigUnsetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Clear one setting from the config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg, err = config.UnsetFileKey(cfg, args[0]); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s cleared\n", args[0])
			return nil
		},
	}
}

// checkPortListen is the file-view of the resolve rule: a port and a
// listen that name different ports cannot be written together.
func checkPortListen(cfg config.Config) error {
	if cfg.Port == nil || cfg.Listen == "" {
		return nil
	}
	_, p, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	if p != strconv.Itoa(*cfg.Port) {
		return fmt.Errorf("port %d conflicts with listen %q: the listen address names a different port", *cfg.Port, cfg.Listen)
	}
	return nil
}

func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// secretState renders a secret's presence ("set", its source, "(unset)")
// without ever the value.
func secretState(v config.Value[string]) string {
	if !v.Set() || v.Value == "" {
		return "(unset)"
	}
	return "set"
}
