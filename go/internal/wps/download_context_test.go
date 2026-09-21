package wps

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type downloadTransportFunc func(*http.Request) (*http.Response, error)

func (f downloadTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadCancellationReachesEachUpstreamPhase(t *testing.T) {
	for _, phase := range []string{"resolve", "object"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			block := func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			var opener Opener = refreshOpenerFunc(block)
			transport := downloadTransportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unexpected object request")
				return nil, errors.New("unexpected")
			})
			if phase == "object" {
				opener = &fakeControlOpener{script: []scriptedResponse{resolveResponse("https://object.ag.kdocs.cn/file")}}
				transport = downloadTransportFunc(block)
			}
			c, err := NewClient(credentialedConfig(), WithOpener(opener), WithSignedTransport(transport))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.OpenDownloadContext(ctx, "file", 0, nil, nil); done <- err }()
			<-entered
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error=%v", err)
			}
		})
	}
}
