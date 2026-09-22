package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestUploadExpectedParentRejectsDifferentFolder(t *testing.T) {
	client := newFakeClient()
	store := newTestStorage(t, client, nil)
	_, err := store.UploadPath(context.Background(), "/docs/new.txt", strings.NewReader("data"), UploadOptions{ExpectedParentID: "other-folder"})
	if !errors.Is(err, ErrUploadTargetChanged) || len(client.uploadCalls) != 0 {
		t.Fatalf("upload escaped pinned parent: %v, writes=%d", err, len(client.uploadCalls))
	}
}

func TestUploadExpectedParentRechecksAfterWaitingForSlot(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*fakeClient)
		expected model.ErrorKind
		changed  bool
	}{
		{"parent replaced", func(c *fakeClient) { c.children["root"][0].ID = "replacement"; c.children["replacement"] = nil }, "", true},
		{"parent removed", func(c *fakeClient) { c.children["root"] = c.children["root"][1:] }, "", true},
		{"destination created", func(c *fakeClient) {
			c.children["docs"] = append(c.children["docs"], model.RemoteEntry{ID: "racing-file", Name: "new.txt", Kind: model.KindFile})
		}, model.KindAlreadyExists, false},
		{"unchanged", func(*fakeClient) {}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := newFakeClient()
				store := newTestStorage(t, client, nil)
				release, err := store.budget.AcquireUpload(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				done := make(chan error, 1)
				go func() {
					_, err := store.UploadPath(context.Background(), "/docs/new.txt", strings.NewReader("data"), UploadOptions{ExpectedParentID: "docs"})
					done <- err
				}()
				synctest.Wait()
				if store.budget.Stats().UploadsWaiting != 1 {
					t.Fatal("upload did not wait for its slot")
				}
				test.mutate(client)
				release()
				err = <-done
				switch {
				case test.changed:
					if !errors.Is(err, ErrUploadTargetChanged) || len(client.uploadCalls) != 0 {
						t.Fatalf("parent change ignored: %v, writes=%d", err, len(client.uploadCalls))
					}
				case test.expected != "":
					storageErr, ok := model.AsStorageError(err)
					if !ok || storageErr.Kind != test.expected || len(client.uploadCalls) != 0 {
						t.Fatalf("new destination overwritten: %v, writes=%d", err, len(client.uploadCalls))
					}
				default:
					if err != nil || len(client.uploadCalls) != 1 || client.uploadCalls[0].parentID != "docs" {
						t.Fatalf("unchanged destination failed: %v, writes=%d", err, len(client.uploadCalls))
					}
				}
			})
		})
	}
}
