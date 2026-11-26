package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"remote.network/client/internal/agent"
	"remote.network/client/internal/config"
	"remote.network/client/internal/nat"
	"remote.network/client/internal/permissions"
	"remote.network/client/internal/proxy"
	"remote.network/client/internal/tools"
	"remote.network/node/clientproto"
	"remote.network/node/identity"
)

func newChatCommand() *cobra.Command {
	var model, apiKey, apiKeyFile, keyFile, router string
	var effort string
	var noDirect, webSearch bool
	cmd := &cobra.Command{
		Use:   "chat --model MODEL \"prompt\"",
		Short: "One-shot completion over the same direct-or-fallback path as serve",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := config.Resolve(config.Flags{
				Router: router, APIKey: apiKey, APIKeyFile: apiKeyFile,
				NoDirect: noDirect, Model: model, WebSearch: webSearch,
			})
			if err != nil {
				return err
			}
			if s.APIKey.Value == "" {
				return errors.New("an API key is required: --api-key-file, the file ~/.remote/client-api-key (run `remote-client login`), or REMOTE_API_KEY")
			}
			if s.DefaultModel.Value == "" {
				return errors.New("a model is required: --model, REMOTE_DEFAULT_MODEL, or default_model in the config file")
			}
			if keyFile == "" {
				keyFile, err = defaultIdentityKeyPath()
				if err != nil {
					return err
				}
			}
			clientKey, _, err := identity.LoadOrCreate(keyFile)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			direct := s.Direct.Value
			var natSock *nat.Socket
			if direct {
				natSock, err = nat.Bind(nat.Config{})
				if err != nil {
					return err
				}
				defer natSock.Close()
				hc := &http.Client{Timeout: 30 * time.Second}
				urls := []string{s.Router.Value}
				if s.RouterAlt.Value != "" {
					urls = append(urls, s.RouterAlt.Value)
				}
				go natSock.Run(ctx, func(rctx context.Context) ([]nat.RouterEndpoints, error) {
					return directInfoEndpoints(rctx, hc, urls)
				})
			}
			srv, err := proxy.New(proxy.Config{
				Router: s.Router.Value, APIKey: s.APIKey.Value, ClientKey: clientKey,
				NAT: natSock, Direct: direct, Version: Version,
				// chat's stderr carries the model's thinking: the proxy's
				// per-request lines stay out of it unless debug is asked for.
				Log: slog.New(slog.NewTextHandler(os.Stderr, chatLogOpts(s.LogLevel.Value))),
			})
			if err != nil {
				return err
			}
			defer srv.Close()

			prompt := ""
			for i, a := range args {
				if i > 0 {
					prompt += " "
				}
				prompt += a
			}
			history := []clientproto.Message{{Role: "user", Content: jsonRawString(prompt)}}

			// One path for every chat: the agent loop, streaming. With MCP
			// servers configured it offers their tools (the proxy never does
			// — a third-party request with its own tools passes through
			// untouched); without, it is one plain round. Either way the
			// answer prints as text while it generates, the model's thinking
			// goes to stderr, and an error is a message, not a JSON body.
			loop := &agent.Loop{Proxy: srv.Handler(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if s.Tools.Value {
				cfg, terr := tools.LoadConfig(s.MCPConfig.Value)
				if terr != nil {
					return fmt.Errorf("the MCP config could not be read (%w) — fix it or run with tools off (REMOTE_TOOLS=off)", terr)
				}
				if len(cfg) > 0 {
					reg := tools.NewRegistry(cfg, loop.Log)
					defer reg.Close()
					perms, perr := permissions.Load()
					if perr != nil {
						perms = permissions.NewMemory()
					}
					loop.Tools, loop.Perms, loop.Ask = reg, perms, newTTYAsker()
				}
			}
			pr := newChatPrinter(cmd)
			_, terr := loop.Turn(ctx, agent.TurnInput{
				Model: s.DefaultModel.Value, Messages: history, Effort: effort,
				WebSearch: s.WebSearch.Value, Stream: true,
			}, pr.event)
			pr.finish()
			return terr
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to ask for (default: config file default_model, env REMOTE_DEFAULT_MODEL)")
	cmd.Flags().StringVar(&router, "router", "", "router base URL (default: config file, env REMOTE_ROUTER, or the public router)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY; prefer the key file)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key)")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key)")
	cmd.Flags().BoolVar(&noDirect, "no-direct", false, "send everything through the router")
	cmd.Flags().BoolVar(&webSearch, "web-search", false, "ground the answer in a router-run web search")
	cmd.Flags().StringVar(&effort, "effort", "", "reasoning effort for thinking models: off, low, medium, high (default: the model's own)")
	return cmd
}

// chatLogOpts is chat's proxy log level: warnings only, unless the
// resolved log level is debug.
func chatLogOpts(level string) *slog.HandlerOptions {
	if level == "debug" {
		return &slog.HandlerOptions{Level: slog.LevelDebug}
	}
	return &slog.HandlerOptions{Level: slog.LevelWarn}
}
