package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"remote.network/client/internal/config"
	"remote.network/client/internal/nat"
	"remote.network/client/internal/proxy"
	"remote.network/node/identity"
)

func newServeCommand() *cobra.Command {
	var router, listen, apiKey, apiKeyFile, localToken, keyFile, routerAlt string
	var noDirect, verbose bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the local OpenAI-compatible endpoint (http://127.0.0.1:4141/v1)",
		Long: "serve runs a local OpenAI-compatible endpoint. Each chat request goes straight\n" +
			"to a serving node over QUIC when a direct path exists, and through a router\n" +
			"otherwise. Point any OpenAI-compatible tool at http://127.0.0.1:4141/v1.\n" +
			"Needs an API key: --api-key-file, ~/.remote/client-api-key (see `login`), or\n" +
			"REMOTE_API_KEY.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if router == "" {
				router = cfg.Router
			}
			if routerAlt == "" {
				routerAlt = cfg.RouterAlt
			}
			if listen == "" {
				listen = cfg.Listen
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
			// A non-loopback listener exposes the endpoint to the network:
			// it needs a local token, compared per request.
			loop, err := isLoopbackListen(listen)
			if err != nil {
				return fmt.Errorf("--listen %s: %w", listen, err)
			}
			if !loop && localToken == "" {
				return errors.New("listening on a non-loopback address requires --local-token (anyone who can reach the port would use your credits)")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			var natSock *nat.Socket
			if direct {
				natSock, err = nat.Bind(nat.Config{})
				if err != nil {
					return err
				}
				defer natSock.Close()
				hc := &http.Client{Timeout: 15 * time.Second}
				urls := []string{router}
				if routerAlt != "" {
					urls = append(urls, routerAlt)
				}
				go natSock.Run(ctx, func(rctx context.Context) ([]nat.RouterEndpoints, error) {
					return directInfoEndpoints(rctx, hc, urls)
				})
			}
			srv, err := proxy.New(proxy.Config{
				Router: router, APIKey: apiKey, LocalToken: localToken,
				ClientKey: clientKey, NAT: natSock, Direct: direct,
				Version: Version,
				Log:     slog.New(slog.NewTextHandler(os.Stderr, logOpts(verbose))),
			})
			if err != nil {
				return err
			}
			defer srv.Close()

			httpSrv := &http.Server{
				Addr:              listen,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			mode := "off"
			if direct {
				mode = "on"
			}
			cmd.Printf("remote-client %s listening on http://%s (direct path: %s)\n", Version, listen, mode)
			cmd.Printf("point your OpenAI-compatible tool at http://%s/v1\n", listen)
			errCh := make(chan error, 1)
			go func() {
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
				}
			}()
			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				return httpSrv.Shutdown(context.Background())
			}
		},
	}
	def := os.Getenv("REMOTE_ROUTER")
	if def == "" {
		def = ""
	}
	cmd.Flags().StringVar(&router, "router", def, "router base URL (default: config file, env REMOTE_ROUTER, or the public router)")
	cmd.Flags().StringVar(&routerAlt, "router-alt", "", "a second router's base URL — strengthens the NAT test")
	cmd.Flags().StringVar(&listen, "listen", "", "local listen address (default 127.0.0.1:4141)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY; prefer the key file)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key)")
	cmd.Flags().StringVar(&localToken, "local-token", "", "secret clients must present when the listener is not loopback-only")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key)")
	cmd.Flags().BoolVar(&noDirect, "no-direct", false, "send everything through the router")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log why each direct attempt was refused or fell back (never logs the key or prompts)")
	return cmd
}

// resolveKey applies the key sources in order: the flag, the named file,
// the default file, the environment. The key is never printed.
func resolveKey(flagVal, fileFlag string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if k, err := config.ReadKeyFile(fileFlag); err != nil || k != "" {
		return k, err
	}
	return os.Getenv("REMOTE_API_KEY"), nil
}

func defaultIdentityKeyPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client.key"), nil
}

// isLoopbackListen reports whether the address only accepts connections
// from this machine.
func isLoopbackListen(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "" {
		return false, nil // all interfaces
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	// A hostname: every address it resolves to must be loopback. An
	// unresolvable host is not loopback (it would need the token) — the
	// safe reading, not an error.
	ips, err := net.LookupHost(host)
	if err != nil {
		return false, nil
	}
	for _, s := range ips {
		if ip := net.ParseIP(s); ip == nil || !ip.IsLoopback() {
			return false, nil
		}
	}
	return true, nil
}

// directInfoEndpoints reads each router's public punch sockets
// (GET /api/v1/direct/info).
func directInfoEndpoints(ctx context.Context, hc *http.Client, urls []string) ([]nat.RouterEndpoints, error) {
	var out []nat.RouterEndpoints
	var lastErr error = errors.New("no router configured")
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimSuffix(u, "/")+"/api/v1/direct/info", nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var doc struct {
			UDPAddr    string `json:"udp_addr"`
			AltUDPAddr string `json:"alt_udp_addr"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		main, err1 := net.ResolveUDPAddr("udp", doc.UDPAddr)
		alt, err2 := net.ResolveUDPAddr("udp", doc.AltUDPAddr)
		if err := errors.Join(err1, err2); err != nil {
			lastErr = err
			continue
		}
		out = append(out, nat.RouterEndpoints{Main: main, Alt: alt})
	}
	if len(out) == 0 {
		return nil, lastErr
	}
	return out, nil
}

// logOpts is the serve log level: --verbose adds the per-candidate
// decisions (dial failures, pre-token refusals and their codes, route
// fallbacks).
func logOpts(verbose bool) *slog.HandlerOptions {
	if verbose {
		return &slog.HandlerOptions{Level: slog.LevelDebug}
	}
	return nil
}
