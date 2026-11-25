package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"remote.network/client/internal/config"
)

func newLoginCommand() *cobra.Command {
	var apiKey, keyFile string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store the API key in ~/.remote/client-api-key (0600)",
		Long:  "login stores the API key in ~/.remote/client-api-key with owner-only permissions, so serve, chat and doctor read it without a flag. Pass --api-key or paste it at the prompt. The key is never printed back.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apiKey == "" {
				fmt.Fprint(cmd.OutOrStdout(), "Paste your API key (rn_sk_…), then press Enter: ")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return err
				}
				apiKey = strings.TrimSpace(line)
			}
			if !strings.HasPrefix(apiKey, "rn_sk_") {
				return errors.New("that does not look like a remote.network API key (expected rn_sk_…); nothing was stored")
			}
			if keyFile == "" {
				var err error
				keyFile, err = config.KeyFilePath()
				if err != nil {
					return err
				}
			}
			if err := config.WriteKeyFile(keyFile, apiKey); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "API key stored in %s (0600).\n", keyFile)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "the key to store (default: read from stdin)")
	cmd.Flags().StringVar(&keyFile, "api-key-file", "", "where to store it (default ~/.remote/client-api-key)")
	return cmd
}
