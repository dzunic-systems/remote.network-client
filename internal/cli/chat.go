package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"remote.network/client/internal/config"
	"remote.network/client/internal/nat"
	"remote.network/client/internal/proxy"
	"remote.network/node/identity"
)

func newChatCommand() *cobra.Command {
	var model, apiKey, apiKeyFile, keyFile, router string
	var noDirect bool
	cmd := &cobra.Command{
		Use:   "chat --model MODEL \"prompt\"",
		Short: "One-shot completion over the same direct-or-fallback path as serve",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if router == "" {
				router = cfg.Router
			}
			direct := cfg.DirectOn() && !noDirect
			apiKey, err = resolveKey(apiKey, apiKeyFile)
			if err != nil {
				return err
			}
			if apiKey == "" {
				return errors.New("an API key is required: --api-key-file, the file ~/.remote/client-api-key (run `remote-client login`), or REMOTE_API_KEY")
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
			var natSock *nat.Socket
			if direct {
				natSock, err = nat.Bind(nat.Config{})
				if err != nil {
					return err
				}
				defer natSock.Close()
				hc := &http.Client{Timeout: 30 * time.Second}
				urls := []string{cfg.Router}
				if cfg.RouterAlt != "" {
					urls = append(urls, cfg.RouterAlt)
				}
				go natSock.Run(ctx, func(rctx context.Context) ([]nat.RouterEndpoints, error) {
					return directInfoEndpoints(rctx, hc, urls)
				})
			}
			srv, err := proxy.New(proxy.Config{
				Router: router, APIKey: apiKey, ClientKey: clientKey,
				NAT: natSock, Direct: direct, Version: Version,
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
			body, _ := json.Marshal(map[string]any{
				"model":    model,
				"messages": []map[string]any{{"role": "user", "content": prompt}},
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/v1/chat/completions", bytes.NewReader(body))
			if err != nil {
				return err
			}
			out := &stdoutWriter{out: cmd.OutOrStdout()}
			srv.Handler().ServeHTTP(out, req)
			if out.code < 200 || out.code >= 300 {
				return fmt.Errorf("request failed (%d)", out.code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to ask for (required)")
	_ = cmd.MarkFlagRequired("model")
	cmd.Flags().StringVar(&router, "router", os.Getenv("REMOTE_ROUTER"), "router base URL (default: config file, env REMOTE_ROUTER, or the public router)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY; prefer the key file)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key)")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key)")
	cmd.Flags().BoolVar(&noDirect, "no-direct", false, "send everything through the router")
	return cmd
}

// stdoutWriter is the http.ResponseWriter the chat command serves one
// request into: status captured, body straight to the terminal.
type stdoutWriter struct {
	out  interface{ Write([]byte) (int, error) }
	code int
	head http.Header
}

func (w *stdoutWriter) Header() http.Header {
	if w.head == nil {
		w.head = http.Header{}
	}
	return w.head
}

func (w *stdoutWriter) WriteHeader(code int) { w.code = code }

func (w *stdoutWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.out.Write(b)
}

func (w *stdoutWriter) Flush() {}
