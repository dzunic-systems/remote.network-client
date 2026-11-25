package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"remote.network/client/internal/doctor"
	"remote.network/node/identity"
)

// DefaultRouter is the public network entry point.
const DefaultRouter = "https://spine.remote.network"

func newDoctorCommand() *cobra.Command {
	var router, apiKey, apiKeyFile, model, node, keyFile string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this machine can reach a node directly",
		Long: "doctor finds this machine's public address and NAT type, asks a router for a node\n" +
			"that accepts direct connections, dials it over QUIC and pings it. It sends no\n" +
			"inference and costs nothing. Needs an API key (--api-key or REMOTE_API_KEY).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apiKey == "" {
				apiKey = os.Getenv("REMOTE_API_KEY")
			}
			if apiKey == "" {
				k, err := readKeyFile(apiKeyFile)
				if err != nil {
					return err
				}
				apiKey = k
			}
			if apiKey == "" {
				return errors.New("an API key is required: --api-key rn_sk_…, REMOTE_API_KEY, or the file ~/.remote/client-api-key (create a key in the dashboard under User Settings › API Keys)")
			}
			if keyFile == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				keyFile = filepath.Join(home, ".remote", "client.key")
			}
			key, _, err := identity.LoadOrCreate(keyFile)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			_, err = doctor.Run(ctx, doctor.Options{
				Router: router, APIKey: apiKey, Model: model, NodeID: node, ClientKey: key, Version: Version,
			}, cmd.OutOrStdout())
			return err
		},
	}
	def := os.Getenv("REMOTE_ROUTER")
	if def == "" {
		def = DefaultRouter
	}
	cmd.Flags().StringVar(&router, "router", def, "router base URL (env REMOTE_ROUTER)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key; keeps the key out of shell history)")
	cmd.Flags().StringVar(&model, "model", "", "check a node serving this model (default: any node that accepts direct connections)")
	cmd.Flags().StringVar(&node, "node", "", "check this node id")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key, created on first use)")
	return cmd
}

// readKeyFile reads an API key file ("" = ~/.remote/client-api-key). A
// missing default file is not an error — it returns "".
func readKeyFile(path string) (string, error) {
	explicit := path != ""
	if !explicit {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".remote", "client-api-key")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
