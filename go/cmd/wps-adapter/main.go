// Command wps-adapter serves the WPS enterprise drive as WebDAV and REST.
//
// Skeleton stage (task B200): only the command shapes --version,
// check-config, and serve exist, and none of them touch WPS. The serve
// command listens and answers /healthz; real routes arrive with the later
// migration tasks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/app"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
)

// Build-time injection points:
//
//	go build -ldflags "-X main.version=0.9.8 -X main.commit=$(git rev-parse --short HEAD)"
var (
	version = "0.9.8"
	commit  = "unknown"
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
			fmt.Println(version)
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
		groupState, authState, cfg.DAVPrefix, cfg.RESTPrefix,
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
	if err := cfg.CheckPublicBind(); err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	maxConnections, requestTimeout, err := config.ParseServerRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	cfg.MaxConnections = maxConnections
	cfg.RequestTimeout = requestTimeout
	if err := cfg.ValidateRuntime(); err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}

	application := &app.Application{Config: cfg, Version: version}
	server := &http.Server{
		Handler: application.Handler(),
	}

	// Handlers must be installed before anything observable (listening
	// lines included): a signal arriving between the printed lines and
	// registration would otherwise kill the process with the default
	// disposition instead of shutting down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		return 1
	}
	fmt.Printf("listening=http://%s:%d\n", cfg.Bind, cfg.Port)
	fmt.Printf(
		"webdav=http://%s:%d%s/ rest=http://%s:%d%s/\n",
		cfg.Bind, cfg.Port, cfg.DAVPrefix,
		cfg.Bind, cfg.Port, cfg.RESTPrefix,
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
		return 0
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
			return 1
		}
		return 0
	}
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
