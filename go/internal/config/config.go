// Package config loads adapter runtime configuration from the environment.
//
// The skeleton stage only carries the fields the serve shape needs; the full
// environment matrix migrates in task B201 and must stay byte-compatible
// with the Python reference (src/wps_adapter/__main__.py).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the skeleton runtime settings.
type Config struct {
	Bind string
	Port int

	Username     string
	Password     string
	UsernameFile string
	PasswordFile string

	DAVPrefix  string
	RESTPrefix string
}

// Load reads the environment and validates the skeleton fields. Error text
// names the variable and the rule, never the value.
func Load() (Config, error) {
	cfg := Config{
		Bind:         os.Getenv("ADAPTER_BIND"),
		Username:     os.Getenv("ADAPTER_USERNAME"),
		Password:     os.Getenv("ADAPTER_PASSWORD"),
		UsernameFile: os.Getenv("ADAPTER_USERNAME_FILE"),
		PasswordFile: os.Getenv("ADAPTER_PASSWORD_FILE"),
		DAVPrefix:    normalisePrefix(os.Getenv("ADAPTER_DAV_PREFIX"), "/dav"),
		RESTPrefix:   normalisePrefix(os.Getenv("ADAPTER_REST_PREFIX"), "/api/v1"),
	}
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	portText := os.Getenv("ADAPTER_PORT")
	if portText == "" {
		cfg.Port = 54321
		return cfg, nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Config{}, fmt.Errorf("ADAPTER_PORT must be an integer")
	}
	if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("ADAPTER_PORT must be between 1 and 65535")
	}
	cfg.Port = port
	return cfg, nil
}

// AuthEnabled reports whether Basic Auth is fully configured, either with
// both literal values or with both secret file paths.
func (c Config) AuthEnabled() bool {
	literal := c.Username != "" && c.Password != ""
	files := c.UsernameFile != "" && c.PasswordFile != ""
	return literal || files
}

// CheckPublicBind rejects a non-local bind on a server without Basic Auth,
// mirroring the Python entrypoint guard.
func (c Config) CheckPublicBind() error {
	local := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
	if !local[c.Bind] && !c.AuthEnabled() {
		return fmt.Errorf("refusing a non-local bind without ADAPTER_USERNAME/PASSWORD or secret files")
	}
	return nil
}

// normalisePrefix adds the leading slash, drops trailing slashes, and maps
// the empty prefix to the default, matching AdapterApplication._normalise_prefix.
func normalisePrefix(value, fallback string) string {
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "/"
	}
	return value
}
