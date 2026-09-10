package wps

import (
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

func TestPersonalModeUsesDriveHostAndDropsBusinessPrefix(t *testing.T) {
	config := DefaultConfig("group-1")
	config.Mode = ModePersonal
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		listResponse(`{"files":[],"next_offset":-1,"result":"ok"}`),
	}}
	client.opener = opener
	if _, err := client.ListEntries("0", ListOptions{}); err != nil {
		t.Fatalf("ListEntries failed: %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.URL.Scheme != "https" || request.URL.Host != "drive.wps.cn" {
		t.Fatalf("url = %q, want drive.wps.cn", request.URL.String())
	}
	if request.URL.Path != "/api/v5/groups/group-1/files" {
		t.Fatalf("path = %q, want personal API path", request.URL.Path)
	}
}

func TestNewClientRejectsUnknownMode(t *testing.T) {
	config := DefaultConfig("group-1")
	config.Mode = "unknown"
	if _, err := NewClient(config); err == nil || err.Error() != "mode must be auto, business, or personal" {
		t.Fatalf("error = %v, want mode validation error", err)
	}
}

func TestAutoModeFollowsWorkspaceAfterLoginImport(t *testing.T) {
	state, err := workspace.NewWorkspaceState("", "group-1", "0")
	if err != nil {
		t.Fatalf("NewWorkspaceState failed: %v", err)
	}
	config := DefaultConfig("")
	config.Mode = ModeAuto
	config.Workspace = state
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		listResponse(`{"files":[],"next_offset":-1,"result":"ok"}`),
		listResponse(`{"files":[],"next_offset":-1,"result":"ok"}`),
	}}
	client.opener = opener
	if _, err := client.ListEntries("0", ListOptions{}); err != nil {
		t.Fatalf("business list failed: %v", err)
	}
	if err := state.UpdateWithPathsMode("group-1", "0", "/", nil, workspace.ModePersonal); err != nil {
		t.Fatalf("personal login import failed: %v", err)
	}
	if _, err := client.ListEntries("0", ListOptions{}); err != nil {
		t.Fatalf("personal list failed: %v", err)
	}
	if got := opener.requests[0].URL.Host; got != "365.kdocs.cn" {
		t.Fatalf("initial host = %q, want enterprise host", got)
	}
	if got := opener.requests[1].URL.Host; got != "drive.wps.cn" {
		t.Fatalf("updated host = %q, want personal host", got)
	}
	if got := opener.requests[1].URL.Path; got != "/api/v5/groups/group-1/files" {
		t.Fatalf("updated path = %q, want personal path", got)
	}
}
