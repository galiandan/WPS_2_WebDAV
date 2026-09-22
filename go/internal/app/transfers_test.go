package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/transfers"
)

func submitScopedArchive(t *testing.T, h *scopedAppHarness, user, path string) transfers.Task {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"kind": "archive", "paths": []string{path}})
	response := h.request("POST", "/api/v1/transfers", user, nil, string(body), nil)
	var result struct {
		Task transfers.Task `json:"task"`
	}
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Task.ID == "" {
		t.Fatalf("submit archive: %d %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response = h.request("GET", "/api/v1/transfers/"+result.Task.ID, user, nil, "", nil)
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("read archive: %d %s", response.Code, response.Body.String())
		}
		if result.Task.State == "completed" && result.Task.ArtifactReady {
			return result.Task
		}
		if result.Task.State == "failed" {
			t.Fatalf("archive failed: %+v", result.Task)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("archive did not finish: %+v", result.Task)
	return transfers.Task{}
}

func TestTransfersUseAuthenticatedScopedRoutesAndPersistArtifacts(t *testing.T) {
	h := newScopedAppHarness(t)
	for _, user := range []string{"alice", "adapter"} {
		path := "/shared.txt"
		if user == "adapter" {
			path = "/alpha/shared.txt"
		}
		task := submitScopedArchive(t, h, user, path)
		target := "/api/v1/transfers/" + task.ID
		for _, suffix := range []string{"", "/download", "/cancel", "/retry"} {
			method := "GET"
			if suffix == "/cancel" || suffix == "/retry" {
				method = "POST"
			}
			response := h.request(method, target+suffix, "bob", nil, "", nil)
			if response.Code != 404 {
				t.Fatalf("foreign transfer: %d %s", response.Code, response.Body.String())
			}
		}
		if response := h.request("GET", target+"/download", "", nil, "", nil); response.Code < 400 {
			t.Fatal("anonymous artifact download")
		}
		// Closing and rebuilding the transfer service retains completed files.
		h.app.Transfers.Close()
		if err := h.app.initTransfers(); err != nil {
			t.Fatal(err)
		}
		response := h.request("GET", target+"/download", user, nil, "", nil)
		reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
		if response.Code != 200 || err != nil || len(reader.File) != 1 {
			t.Fatalf("artifact: status=%d err=%v", response.Code, err)
		}
		stream, err := reader.File[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil || string(data) != "content-201" {
			t.Fatalf("scoped artifact=%q err=%v", data, err)
		}
	}
	response := h.request("GET", "/api/v1/transfers", "bob", nil, "", nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), "shared.txt") {
		t.Fatalf("foreign history: %d %s", response.Code, response.Body.String())
	}
}

func TestReadOnlyTransfersAllowArchivesAndRejectFetch(t *testing.T) {
	h := newScopedAppHarness(t)
	permissions := auth.Permissions{Read: true}
	if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	submitScopedArchive(t, h, "alice", "/shared.txt")
	response := h.request("POST", "/api/v1/transfers", "alice", nil, `{"kind":"fetch","url":"https://example.com/file","destination":"/new.txt"}`, nil)
	if response.Code != 403 {
		t.Fatalf("read-only fetch: %d %s", response.Code, response.Body.String())
	}
}

func TestTransferArtifactRevokedByPolicyAndStorageChanges(t *testing.T) {
	for _, change := range []string{"policy", "root"} {
		t.Run(change, func(t *testing.T) {
			h := newScopedAppHarness(t)
			task := submitScopedArchive(t, h, "alice", "/shared.txt")
			if change == "policy" {
				permissions := auth.Permissions{Read: true}
				if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Permissions: &permissions}); err != nil {
					t.Fatal(err)
				}
			} else {
				h.remote.mu.Lock()
				h.remote.children["0"][0]["id"] = "999"
				h.remote.mu.Unlock()
			}
			response := h.request("GET", "/api/v1/transfers/"+task.ID+"/download", "alice", nil, "", nil)
			if response.Code < 400 {
				t.Fatalf("artifact survived %s", change)
			}
		})
	}
}
