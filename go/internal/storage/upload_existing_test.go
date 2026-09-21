package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestUpdateOnlyUploadCannotCreateOrReplaceDifferentFile(t *testing.T) {
	for _, target := range []string{"missing.txt", "top.txt", "docs"} {
		client := newFakeClient()
		store := newTestStorage(t, client, nil)
		_, err := store.UploadPath(context.Background(), "/"+target, strings.NewReader("new"), UploadOptions{Overwrite: true, ExpectedID: "different-id"})
		if !errors.Is(err, ErrUploadTargetChanged) || len(client.uploadCalls) != 0 {
			t.Fatalf("target %s: %v; writes=%d", target, err, len(client.uploadCalls))
		}
	}
}

func TestUpdateOnlyUploadRechecksTargetAfterWaitingForBudget(t *testing.T) {
	for _, replacement := range []string{"", "replacement"} {
		client := newFakeClient()
		store := newTestStorage(t, client, nil)
		release, err := store.budget.AcquireUpload(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		done := make(chan error, 1)
		go func() {
			_, err := store.UploadPath(context.Background(), "/top.txt", strings.NewReader("new"), UploadOptions{Overwrite: true, ExpectedID: "top"})
			done <- err
		}()
		deadline := time.After(time.Second)
		for store.budget.Stats().UploadsWaiting == 0 {
			select {
			case err := <-done:
				t.Fatalf("upload never waited: %v", err)
			case <-deadline:
				t.Fatal("upload did not wait for slot")
			case <-time.After(time.Millisecond):
			}
		}
		// The worker has finished its cached lookup and is blocked on the
		// budget. Mutate the fake upstream before releasing that slot.
		client.children["root"] = nil
		if replacement != "" {
			client.children["root"] = []model.RemoteEntry{{ID: replacement, Name: "top.txt", Kind: model.KindFile}}
		}
		release()
		if err := <-done; !errors.Is(err, ErrUploadTargetChanged) || len(client.uploadCalls) != 0 {
			t.Fatalf("changed while queued: %v; writes=%d", err, len(client.uploadCalls))
		}
	}
}
