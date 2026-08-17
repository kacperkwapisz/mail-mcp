// Command mail-mcp serves IMAP and SMTP mailboxes to AI agents over MCP.
//
// Credentials live in a server-side config file and never reach the client:
// tools take an opaque account_id, and the server resolves it locally.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/httpx"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/tools"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=1.0.0"
var version = "dev"

const mcpPath = "/mcp"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mail-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", envOr("CONFIG_PATH", "config.yml"), "path to the YAML config file")
		addr        = flag.String("addr", envOr("ADDR", ":"+envOr("PORT", "3000")), "address to listen on")
		transport   = flag.String("transport", envOr("TRANSPORT", "http"), "transport: http or stdio")
		logLevel    = flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level: debug, info, warn, error")
		trustProxy  = flag.Bool("trust-proxy", envBool("TRUST_PROXY", false), "trust X-Forwarded-For for rate limiting; only enable behind a proxy you control")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mail-mcp", version)
		return nil
	}

	logger := newLogger(*logLevel, *transport)
	mailbox.Version = version

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger.Info("configuration loaded",
		"path", *configPath,
		"accounts", len(cfg.Accounts),
		"send_enabled", cfg.AllowSend,
		"delete_enabled", cfg.AllowDelete,
	)

	pool := mailbox.NewPool(cfg, logger)
	defer pool.Close()

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "mail-mcp", Version: version, Title: "Mail"},
		&mcp.ServerOptions{Instructions: tools.Instructions, Logger: logger},
	)
	tools.New(cfg, pool, logger, version).Register(srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch strings.ToLower(*transport) {
	case "stdio":
		logger.Info("serving on stdio", "version", version)
		return srv.Run(ctx, &mcp.StdioTransport{})
	case "http":
		return serveHTTP(ctx, srv, logger, *addr, *trustProxy)
	default:
		return fmt.Errorf("unknown transport %q: use http or stdio", *transport)
	}
}

func serveHTTP(ctx context.Context, srv *mcp.Server, logger *slog.Logger, addr string, trustProxy bool) error {
	// No token, no server. This process can read and send a person's mail;
	// starting it open to the network would be indefensible.
	apiKey := strings.TrimSpace(os.Getenv("MCP_API_KEY"))
	if apiKey == "" {
		return errors.New("MCP_API_KEY is not set. The HTTP transport requires a bearer token — " +
			"generate one with `openssl rand -hex 32`, or use --transport stdio for local-only use")
	}
	if len(apiKey) < 16 {
		return errors.New("MCP_API_KEY is too short; use at least 16 characters (`openssl rand -hex 32`)")
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Logger: logger, Stateless: true},
	)

	limiter := httpx.NewRateLimiter(
		envInt("RATE_LIMIT_GET_RPM", 60),
		envInt("RATE_LIMIT_POST_RPM", 240),
		trustProxy,
	)

	// Outermost first: unknown paths die before anything else runs, and
	// authentication happens before rate-limit state is touched.
	var wrapped http.Handler = handler
	wrapped = httpx.RequireBearer(apiKey, logger, wrapped)
	wrapped = limiter.Middleware(wrapped)
	wrapped = httpx.SecurityHeaders(wrapped)
	wrapped = httpx.LogRequests(logger, wrapped)
	wrapped = httpx.OnlyPath(mcpPath, wrapped)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "path", mcpPath, "version", version)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// newLogger writes to stderr, which keeps stdout clean for the stdio
// transport's JSON-RPC framing.
func newLogger(level, transport string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if transport == "stdio" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(envOr(key, "")); err == nil {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(envOr(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
