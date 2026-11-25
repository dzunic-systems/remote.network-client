package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"remote.network/client/internal/config"
	"remote.network/client/internal/doctor"
	"remote.network/node/identity"
)

func newDoctorCommand() *cobra.Command {
	var router, apiKey, apiKeyFile, model, node, keyFile string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this machine can reach a node directly",
		Long: "doctor finds this machine's public address and NAT type, asks a router for a node\n" +
			"that accepts direct connections, dials it over QUIC and pings it. It sends no\n" +
			"inference and costs nothing. Needs an API key (--api-key or REMOTE_API_KEY).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := config.Resolve(config.Flags{Router: router, APIKey: apiKey, APIKeyFile: apiKeyFile})
			if err != nil {
				return err
			}
			if s.APIKey.Value == "" {
				return errors.New("an API key is required: --api-key rn_sk_…, REMOTE_API_KEY, or the file ~/.remote/client-api-key (create a key in the dashboard under User Settings › API Keys)")
			}
			if keyFile == "" {
				keyFile, err = defaultIdentityKeyPath()
				if err != nil {
					return err
				}
			}
			key, _, err := identity.LoadOrCreate(keyFile)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			_, err = doctor.Run(ctx, doctor.Options{
				Router: s.Router.Value, APIKey: s.APIKey.Value, Model: model, NodeID: node, ClientKey: key, Version: Version,
			}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&router, "router", "", "router base URL (default: config file, env REMOTE_ROUTER, or the public router)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key; keeps the key out of shell history)")
	cmd.Flags().StringVar(&model, "model", "", "check a node serving this model (default: any node that accepts direct connections)")
	cmd.Flags().StringVar(&node, "node", "", "check this node id")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key, created on first use)")
	return cmd
}
