package wps

import (
	"net/http"
	"testing"
	"time"
)

// The upstream transports must keep a warm idle pool: the default transport
// caches only two connections per host, which pushes the web UI's parallel
// refreshes and multi-space traffic back into TLS handshakes.
func TestUpstreamTransportsKeepWarmIdlePool(t *testing.T) {
	client := NewControlHTTPClient(30)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("control transport = %T, want *http.Transport", client.Transport)
	}
	if transport.MaxIdleConns != upstreamMaxIdleConns {
		t.Errorf("control MaxIdleConns = %d, want %d", transport.MaxIdleConns, upstreamMaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost {
		t.Errorf("control MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, upstreamMaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != upstreamIdleConnTimeout {
		t.Errorf("control IdleConnTimeout = %s, want %s", transport.IdleConnTimeout, upstreamIdleConnTimeout)
	}

	signed := NewSignedTransport(30)
	signedTransport, ok := signed.(*http.Transport)
	if !ok {
		t.Fatalf("signed transport = %T, want *http.Transport", signed)
	}
	if signedTransport.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost {
		t.Errorf("signed MaxIdleConnsPerHost = %d, want %d", signedTransport.MaxIdleConnsPerHost, upstreamMaxIdleConnsPerHost)
	}
	if signedTransport.IdleConnTimeout != 90*time.Second {
		t.Errorf("signed IdleConnTimeout = %s, want 90s", signedTransport.IdleConnTimeout)
	}
}
