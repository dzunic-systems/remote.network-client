package cli

import (
	"bytes"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"remote.network/client/internal/agent"
	"remote.network/client/internal/config"
	"remote.network/client/internal/doctor"
	"remote.network/client/internal/nat"
	"remote.network/client/internal/permissions"
	"remote.network/client/internal/proxy"
	"remote.network/client/internal/tools"
	"remote.network/client/internal/ui"
	"remote.network/client/web"
	"remote.network/node/identity"
)

func newServeCommand() *cobra.Command {
	var router, routerAlt, listen, apiKey, apiKeyFile, localToken, keyFile string
	var port int
	var noDirect, verbose, noUI bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the local OpenAI-compatible endpoint (http://127.0.0.1:4141/v1)",
		Long: "serve runs a local OpenAI-compatible endpoint. Each chat request goes straight\n" +
			"to a serving node over QUIC when a direct path exists, and through a router\n" +
			"otherwise. Point any OpenAI-compatible tool at http://127.0.0.1:4141/v1.\n" +
			"A small web UI lives at http://127.0.0.1:4141/ui/ (chat, settings, a doctor\n" +
			"check). Needs an API key: --api-key-file, ~/.remote/client-api-key (see\n" +
			"`login`), or REMOTE_API_KEY.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fl := config.Flags{
				Router: router, RouterAlt: routerAlt, Listen: listen, Port: port,
				APIKey: apiKey, APIKeyFile: apiKeyFile, NoDirect: noDirect,
				LocalToken: localToken, Verbose: verbose, NoUI: noUI,
			}
			s, err := config.Resolve(fl)
			if err != nil {
				return err
			}
			if s.APIKey.Value == "" {
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
			loop, err := isLoopbackListen(s.Listen.Value)
			if err != nil {
				return fmt.Errorf("--listen %s: %w", s.Listen.Value, err)
			}
			if !loop && s.LocalToken.Value == "" {
				return errors.New("listening on a non-loopback address requires --local-token (anyone who can reach the port would use your credits)")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			direct := s.Direct.Value
			var natSock *nat.Socket
			if direct {
				natSock, err = nat.Bind(nat.Config{})
				if err != nil {
					return err
				}
				defer natSock.Close()
				hc := &http.Client{Timeout: 15 * time.Second}
				urls := []string{s.Router.Value}
				if s.RouterAlt.Value != "" {
					urls = append(urls, s.RouterAlt.Value)
				}
				go natSock.Run(ctx, func(rctx context.Context) ([]nat.RouterEndpoints, error) {
					return directInfoEndpoints(rctx, hc, urls)
				})
			}

			// The web UI: settings (with their sources), Replace key, the
			// doctor check, and the local tools (MCP servers + the agent
			// loop) — all backed by this run's resolution, so a flag-locked
			// setting shows as locked here too.
			var uiHandler http.Handler
			var uiLoop *agent.Loop                  // set inside the UI block; wired to the proxy below
			var srvRef atomic.Pointer[proxy.Server] // set once the proxy exists; the UI's key hand-off
			curKey := func() string {
				if p := srvRef.Load(); p != nil {
					return p.APIKey()
				}
				return s.APIKey.Value
			}
			if s.UI.Value {
				// The tool loop is the UI's chat surface; the registry
				// holds the configured MCP servers (a broken mcp.json does
				// not stop serve — the first turn surfaces it).
				uiLog := slog.New(slog.NewTextHandler(os.Stderr, logOpts(s.LogLevel.Value)))
				var reg *tools.Registry
				if cfg, terr := tools.LoadConfig(s.MCPConfig.Value); terr != nil {
					uiLog.Warn("the MCP config could not be read — no local tools this run", "err", terr)
				} else if len(cfg) == 0 {
					// no servers configured
				} else {
					reg = tools.NewRegistry(cfg, uiLog)
					defer reg.Close()
				}
				perms, perr := permissions.Load()
				if perr != nil {
					uiLog.Warn("the permissions file could not be read — every tool will ask", "err", perr)
					perms = permissions.NewMemory()
				}
				if reg != nil && !reg.Empty() && s.Tools.Value {
					uiLoop = &agent.Loop{
						Perms: perms, Tools: reg, Log: uiLog,
						LocalToken: s.LocalToken.Value,
					}
				}
				toolsOn := func() bool { return uiLoop != nil }
				uiHandler = ui.New(ui.Config{
					Dist:     web.Dist(),
					Settings: func() (config.Settings, error) { return config.Resolve(fl) },
					KeyFile:  s.APIKeyFile.Value,
					OnKey: func(k string) {
						if p := srvRef.Load(); p != nil {
							p.SetAPIKey(k)
						}
					},
					Loop: uiLoop, Reg: reg, Perms: perms, ToolsOn: toolsOn,
					Doctor: func(rctx context.Context) (string, bool) {
						var buf bytes.Buffer
						res, derr := doctor.Run(rctx, doctor.Options{
							Router: s.Router.Value, APIKey: curKey(),
							ClientKey: clientKey, Version: Version,
						}, &buf)
						_ = res
						return buf.String(), derr == nil
					},
				})
			}

			srv, err := proxy.New(proxy.Config{
				Router: s.Router.Value, APIKey: s.APIKey.Value, LocalToken: s.LocalToken.Value,
				ClientKey: clientKey, NAT: natSock, Direct: direct,
				Version: Version, UI: uiHandler,
				Log: slog.New(slog.NewTextHandler(os.Stderr, logOpts(s.LogLevel.Value))),
			})
			if err != nil {
				return err
			}
			srvRef.Store(srv)
			// The tool loop drives its rounds through the proxy's own
			// handler (in-process), so each round takes its own direct or
			// fallback path.
			if uiLoop != nil {
				uiLoop.Proxy = srv.Handler()
			}
			defer srv.Close()

			httpSrv := &http.Server{
				Addr:              s.Listen.Value,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			mode := "off"
			if direct {
				mode = "on"
			}
			cmd.Printf("remote-client %s listening on http://%s (direct path: %s)\n", Version, s.Listen.Value, mode)
			cmd.Printf("point your OpenAI-compatible tool at http://%s/v1\n", s.Listen.Value)
			if s.UI.Value {
				cmd.Printf("the web UI is at http://%s/ui/\n", s.Listen.Value)
			}
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
	cmd.Flags().StringVar(&router, "router", "", "router base URL (default: config file, env REMOTE_ROUTER, or the public router)")
	cmd.Flags().StringVar(&routerAlt, "router-alt", "", "a second router's base URL — strengthens the NAT test")
	cmd.Flags().StringVar(&listen, "listen", "", "local listen address (default 127.0.0.1:4141)")
	cmd.Flags().IntVar(&port, "port", 0, "local listen port, kept on the listen host (default 4141; env REMOTE_PORT)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (env REMOTE_API_KEY; prefer the key file)")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "file holding the API key (default ~/.remote/client-api-key)")
	cmd.Flags().StringVar(&localToken, "local-token", "", "secret clients must present when the listener is not loopback-only")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "this client's identity key (default ~/.remote/client.key)")
	cmd.Flags().BoolVar(&noDirect, "no-direct", false, "send everything through the router")
	cmd.Flags().BoolVar(&noUI, "no-ui", false, "do not serve the web UI at /ui/")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log why each direct attempt was refused or fell back (never logs the key or prompts)")
	return cmd
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

// logOpts is the serve log level (log_level: debug|info|warn; --verbose
// means debug): debug adds the per-candidate decisions (dial failures,
// pre-token refusals and their codes, route fallbacks).
func logOpts(level string) *slog.HandlerOptions {
	switch level {
	case "debug":
		return &slog.HandlerOptions{Level: slog.LevelDebug}
	case "warn":
		return &slog.HandlerOptions{Level: slog.LevelWarn}
	default:
		return nil
	}
}
