// Command contractsrv runs the assembled adapter against the contract test
// relay. It is test infrastructure, never shipped: production builds use
// cmd/wps-adapter, which constructs real WPS transports.
//
// The Python contract harness patches the Python client constructor with an
// in-process fake upstream (python_service.py). The Go assembly cannot be
// patched from outside the process, so this entrypoint passes the same fake
// through a loopback relay: every WPS-shaped request the adapters make is
// serialized to the relay, replayed against the identical FakeUpstream, and
// the answer is rebuilt as an HTTP response. Configuration parsing, secure
// file handling, storage routing, and the server lifecycle stay on the
// production path.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/app"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
)

var version = "0.9.8"

func main() {
	portFlag := flag.Int("port", 0, "listen port (overrides ADAPTER_PORT)")
	relayURL := flag.String("relay", "", "contract fake upstream relay URL")
	flag.Parse()
	if *relayURL == "" {
		fmt.Fprintln(os.Stderr, "contractsrv: --relay is required")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	// The production entrypoint hardcodes /etc/wps-adapter/secrets for web
	// settings; tests redirect that to a private temp directory, the same
	// redirect python_service.py applies.
	if override := os.Getenv("CONTRACT_WEB_SETTINGS_FILE"); override != "" {
		cfg.WebSettingsDir = override
	}
	// The serve-only values and the bind refusal mirror the production main
	// path: the contract service differs from cmd/wps-adapter only in the
	// injected WPS transports.
	maxConnections, requestTimeout, err := config.ParseServerRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	cfg.MaxConnections = maxConnections
	cfg.RequestTimeout = requestTimeout
	if err := cfg.CheckPublicBind(); err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	timeout := secondsToDuration(cfg.Timeout)

	application, err := app.New(cfg, version, app.WithTransports(
		relayOpener{relay: *relayURL, timeout: timeout},
		relayTransport{relay: *relayURL, timeout: timeout},
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	defer application.Close()
	handler, err := application.Handler()
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	port := *portFlag
	if port == 0 {
		port = cfg.Port
	}
	listener, server, err := httpserver.Listen(httpserver.ServerConfig{
		Bind:           "127.0.0.1",
		Port:           port,
		RequestTimeout: timeout,
		TransferBudget: application.Budget,
		Handler:        handler,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("listening=http://127.0.0.1:%d\n", port)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "adapter failed: %v\n", err)
		os.Exit(1)
	}
}

func secondsToDuration(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

// relayRequest sends one serialized WPS-shaped request to the relay and
// rebuilds the answer as an HTTP response.
func relayRequest(relay string, timeout time.Duration, payload map[string]any) (*http.Response, error) {
	payload["timeout"] = timeout.Seconds()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 120 * time.Second}
	response, err := client.Post(relay, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var answer struct {
		Status  int        `json:"status"`
		Headers [][]string `json:"headers"`
		BodyB64 string     `json:"body_b64"`
		Error   string     `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return nil, err
	}
	if answer.Error == "timeout" {
		return nil, fmt.Errorf("relay upstream timed out")
	}
	if answer.Error != "" {
		return nil, fmt.Errorf("relay upstream failed")
	}
	data, err := base64.StdEncoding.DecodeString(answer.BodyB64)
	if err != nil {
		return nil, err
	}
	out := &http.Response{
		StatusCode:    answer.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
	}
	for _, pair := range answer.Headers {
		if len(pair) == 2 {
			out.Header.Add(pair[0], pair[1])
		}
	}
	return out, nil
}

func serializeHeaders(header http.Header) [][]string {
	pairs := make([][]string, 0, len(header))
	for name, values := range header {
		for _, value := range values {
			pairs = append(pairs, []string{name, value})
		}
	}
	return pairs
}

// readRequestBody serializes the request body; control GETs carry a nil
// Body, which the real transport reads as empty.
func readRequestBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	return io.ReadAll(request.Body)
}

// relayOpener stands in for the control-plane transport.
type relayOpener struct {
	relay   string
	timeout time.Duration
}

func (o relayOpener) Do(request *http.Request) (*http.Response, error) {
	body, err := readRequestBody(request)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"method":   request.Method,
		"url":      request.URL.String(),
		"headers":  serializeHeaders(request.Header),
		"body_b64": base64.StdEncoding.EncodeToString(body),
	}
	response, err := relayRequest(o.relay+"/open", o.timeout, payload)
	if err != nil {
		return nil, err
	}
	response.Request = request
	return response, nil
}

// relayTransport stands in for the signed-object transport. Object GETs go
// through the fake's opener path (like Python's downloads); PUT/POST go
// through its raw signed connection, which records every header and hashes
// the body.
type relayTransport struct {
	relay   string
	timeout time.Duration
}

func (t relayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := readRequestBody(request)
	if err != nil {
		return nil, err
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	payload := map[string]any{
		"method":   request.Method,
		"headers":  serializeHeaders(request.Header),
		"body_b64": base64.StdEncoding.EncodeToString(body),
		"timeout":  t.timeout.Seconds(),
	}
	if request.Method == http.MethodGet {
		payload["url"] = request.URL.String()
		return relayRequest(t.relay+"/open", t.timeout, payload)
	}
	payload["target"] = request.URL.RequestURI()
	payload["host"] = request.URL.Hostname()
	if portText := request.URL.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err == nil {
			payload["port"] = port
		}
	}
	response, err := relayRequest(t.relay+"/signed", t.timeout, payload)
	if err != nil {
		return nil, err
	}
	response.Request = request
	return response, nil
}
