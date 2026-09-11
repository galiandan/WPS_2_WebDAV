// Command wps-adapter serves WPS cloud storage as WebDAV and REST.
//
// The command shapes --version, check-config, and serve mirror Python's
// __main__.py. check-config runs the full service assembly offline: it
// builds every local resource but never dials WPS. serve listens behind
// the process-wide connection budget and shuts down gracefully.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/app"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
)

// Build-time injection points:
//
//	go build -trimpath -ldflags "-X main.version=1.0.6 -X main.commit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
var (
	version   = "1.0.6"
	commit    = "unknown"
	buildTime = "unknown"
)

const usage = `Usage: wps-adapter [--version] <command> [flags]

Commands:
  serve          start the WebDAV/REST server (--bind, --port)
  check-config   validate configuration without network calls
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	for _, arg := range args {
		if arg == "--version" {
			// The bare version stays the first token so release checks can
			// compare it directly; the commit and build time complete the
			// non-sensitive build summary the outline requires (07 §8.2).
			fmt.Printf("%s commit=%s build_time=%s\n", version, commit, buildTime)
			return 0
		}
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "check-config":
		return runCheckConfig()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", args[0], usage)
		return 2
	}
}

func loadConfig() (config.Config, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return config.Config{}, 1
	}
	return cfg, 0
}

func runCheckConfig() int {
	cfg, code := loadConfig()
	if code != 0 {
		return code
	}
	// Python's check-config builds the whole AdapterApplication first: the
	// settings file, workspace state, credential source, and client
	// transports are all constructed locally, and only create_server is
	// skipped. The Go assembly does the same without any network traffic.
	application, err := app.New(cfg, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	defer application.Close()
	authState := "disabled"
	if cfg.AuthEnabled() {
		authState = "enabled"
	}
	groupState := "pending-login"
	if cfg.ResolvedGroupID() != "" {
		groupState = "ready"
	}
	fmt.Printf(
		"config=ok group_id=%s auth=%s dav=%s rest=%s\n",
		groupState, authState, application.DAVPrefix(), application.RESTPrefix(),
	)
	return 0
}

func runServe(args []string) int {
	cfg, code := loadConfig()
	if code != 0 {
		return code
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.Bind, "bind", cfg.Bind, "listen address")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	// The runtime values feed the process-wide budget, so they are parsed
	// before the assembly; Python evaluates them at create_server time and
	// the only observable difference is which error wins when several are
	// broken at once.
	maxConnections, requestTimeout, err := config.ParseServerRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	cfg.MaxConnections = maxConnections
	cfg.RequestTimeout = requestTimeout

	application, err := app.New(cfg, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	if err := cfg.CheckPublicBind(); err != nil {
		application.Close()
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	if err := cfg.ValidateRuntime(); err != nil {
		application.Close()
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	handler, err := application.Handler()
	if err != nil {
		application.Close()
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}

	listener, server, err := httpserver.Listen(httpserver.ServerConfig{
		Bind:           cfg.Bind,
		Port:           cfg.Port,
		RequestTimeout: secondsDuration(requestTimeout),
		TransferBudget: application.Budget,
		Handler:        handler,
	})
	if err != nil {
		application.Close()
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}

	// Handlers must be installed before anything observable (listening
	// lines included): a signal arriving between the printed lines and
	// registration would otherwise kill the process with the default
	// disposition instead of shutting down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("listening=http://%s:%d\n", cfg.Bind, cfg.Port)
	fmt.Printf(
		"webdav=http://%s:%d%s/ rest=http://%s:%d%s/\n",
		cfg.Bind, cfg.Port, application.DAVPrefix(),
		cfg.Bind, cfg.Port, application.RESTPrefix(),
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		// Stop accepting connections, then drain with a deadline. Like the
		// Python service, a signal-initiated stop is a normal stop: even a
		// forced shutdown still exits 0.
		if err := shutdownServer(server, shutdownTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "adapter shutdown forced: %v\n", err)
		}
		application.Close()
		return 0
	case err := <-serveErr:
		application.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
			return 1
		}
		return 0
	}
}

func secondsDuration(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

// shutdownTimeout bounds the graceful drain after SIGINT/SIGTERM.
const shutdownTimeout = 10 * time.Second

// shutdownServer stops new connections and waits up to the deadline for the
// active ones; on deadline expiry the leftovers are force-closed.
func shutdownServer(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		if closeErr := server.Close(); closeErr != nil {
			return closeErr
		}
		return err
	}
	return nil
}
