package httpserver

import (
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/tasks"
)

func TestMemberTaskRoutesHideForeignAndOldPolicyRecords(t *testing.T) {
	fake := &taskStorageFake{batchStorageFake: &batchStorageFake{entries: map[string]model.RemoteEntry{"/file": fileEntry("file-id", "file")}}}
	admin := taskHTTPDispatcher(t, fake, nil)
	member := batchDispatcher(t, fake.batchStorageFake)
	member.tasks = admin.tasks
	member.taskOwnerID = strings.Repeat("a", 32)
	member.taskPolicyVersion = 2
	for _, spec := range []tasks.Spec{
		{Operation: "delete", Identity: "01" + strings.Repeat("00", 31), Sources: []tasks.Binding{{Path: "/file", ID: "file-id"}}},
	} {
		created, err := admin.tasks.Submit(spec)
		if err != nil {
			t.Fatal(err)
		}
		awaitHTTPTask(t, admin, created.ID)
		for _, tc := range []struct{ method, suffix string }{{"GET", ""}, {"POST", "/cancel"}, {"POST", "/retry"}} {
			response, _ := taskHTTP(t, member, tc.method, "/api/v1/tasks/"+created.ID+tc.suffix, "")
			if response.Code != 404 || strings.Contains(response.Body.String(), "file-id") {
				t.Fatalf("foreign task leaked %d %s", response.Code, response.Body.String())
			}
		}
	}
	response, _ := taskHTTP(t, member, "GET", "/api/v1/tasks", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "/file") {
		t.Fatalf("foreign names listed: %d %s", response.Code, response.Body.String())
	}
}
