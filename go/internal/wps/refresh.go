package wps

import (
	"context"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
)

// RefreshCoordinator merges overlapping refresh attempts. Share one instance
// across clients backed by the same credential source, never across accounts.
// Its zero value is ready for use.
type RefreshCoordinator struct {
	mu     sync.Mutex
	active *refreshAttempt
}

type refreshAttempt struct {
	done      chan struct{}
	refreshed bool
	err       error
}

// WithRefreshCoordinator coordinates the complete helper/grant/store sequence,
// including mounted clients which otherwise have independent lifetimes.
func WithRefreshCoordinator(coordinator *RefreshCoordinator) Option {
	return func(c *Client) {
		if coordinator != nil {
			c.refreshCoordinator = coordinator
		}
	}
}

func (c *Client) refreshCredentials(ctx context.Context, rejected credentials.Credentials) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	coordinator := c.refreshCoordinator
	coordinator.mu.Lock()
	if active := coordinator.active; active != nil {
		coordinator.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-active.done:
			return active.refreshed, active.err
		}
	}
	attempt := &refreshAttempt{done: make(chan struct{})}
	coordinator.active = attempt
	coordinator.mu.Unlock()

	attempt.refreshed, attempt.err = c.refreshRejectedCredentials(ctx, rejected)
	coordinator.mu.Lock()
	coordinator.active = nil
	close(attempt.done)
	coordinator.mu.Unlock()
	return attempt.refreshed, attempt.err
}

func (c *Client) refreshRejectedCredentials(ctx context.Context, rejected credentials.Credentials) (bool, error) {
	// A late 401 for an older snapshot must retry the already rotated session,
	// not issue another grant against the new refresh token.
	current, err := c.currentCredentials()
	if err != nil {
		return false, err
	}
	if current.Cookie != "" && current != rejected {
		return true, nil
	}
	if c.config.CredentialSource != nil {
		refreshed, err := c.config.CredentialSource.Refresh()
		if err != nil || refreshed {
			return refreshed, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !c.config.AutoRefresh {
		return false, nil
	}
	return c.refreshWPSSession(ctx)
}
