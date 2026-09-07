// The task poller tests replay the progress wire shapes captured in the
// smoke suite: finish/status success fields, failed_list, failed statuses,
// result failures, and the timeout surface. Context cancellation stops the
// sleep immediately and a pre-canceled context never issues a request.

package wps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// progressOpener answers every poll with one constant response, so timeout
// and cancellation tests run to their deadline deterministically.
type progressOpener struct {
	requests []*http.Request
	status   int
	body     []byte
}

func (o *progressOpener) Do(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		request.Body.Close()
	}
	o.requests = append(o.requests, request)
	return &http.Response{
		StatusCode:    o.status,
		Status:        fmt.Sprintf("%d %s", o.status, http.StatusText(o.status)),
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(o.body)),
		ContentLength: int64(len(o.body)),
	}, nil
}

func runningProgress() []byte {
	return []byte(`{"finish":0,"result":"ok","status":"processing","taskuuid":"task-uuid"}`)
}

func successProgress() []byte {
	return []byte(`{"estimated_time_left":-1,"failed_list":null,"finish":1,` +
		`"result":"ok","status":"success","taskid":13,"taskuuid":"task-uuid","total":1}`)
}

func TestWaitForTaskPollsUntilFinish(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: runningProgress()},
		{status: 200, body: successProgress()},
	}}
	client := newWriteClient(t, opener, nil)

	if err := client.WaitForTask(context.Background(), "task-uuid", "move file",
		time.Millisecond, DefaultTaskPollTimeout); err != nil {
		t.Fatalf("WaitForTask failed: %v", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("polls = %d, want 2", len(opener.requests))
	}
	request := opener.requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	if request.URL.Path != "/3rd/drive/api/v5/files/batch/task/progress" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || query.Get("taskuuid") != "task-uuid" {
		t.Fatalf("query = %v, err %v", query, err)
	}
}

func TestWaitForTaskReturnsOnStatusSuccess(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"finish":0,"failed_list":null,"result":"ok","status":"success"}`)},
	}}
	client := newWriteClient(t, opener, nil)

	if err := client.WaitForTask(context.Background(), "task-uuid", "delete file",
		0, DefaultTaskPollTimeout); err != nil {
		t.Fatalf("WaitForTask failed: %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("polls = %d, want 1", len(opener.requests))
	}
}

func TestWaitForTaskFailedListSurfaces409(t *testing.T) {
	cases := []struct {
		name       string
		failedList string
	}{
		{"list", `[{"id":7,"name":"gone.txt"}]`},
		{"string", `"x"`},
		{"false", `false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				{status: 200, body: []byte(`{"failed_list":` + tc.failedList + `,"finish":1,"result":"ok","status":"success"}`)},
			}}
			client := newWriteClient(t, opener, nil)

			err := client.WaitForTask(context.Background(), "task-uuid", "move file", 0, time.Second)
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Operation != "move file" || apiErr.Status != 409 ||
				apiErr.Category != model.WpsCategoryUpstream {
				t.Fatalf("error = %v, want the 409 move-file failure", err)
			}
			if err.Error() != "WPS operation failed: move file (HTTP 409)" {
				t.Fatalf("message = %q, want the redacted 409 text", err.Error())
			}
		})
	}
}

func TestWaitForTaskFailedStatusSurfacesTaskError(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"failed", "failed"},
		{"error", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				{status: 200, body: []byte(`{"finish":0,"result":"ok","status":"` + tc.status + `"}`)},
			}}
			client := newWriteClient(t, opener, nil)

			err := client.WaitForTask(context.Background(), "task-uuid", "delete file", 0, time.Second)
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Operation != "delete file task" || apiErr.Status != 0 ||
				apiErr.Category != model.WpsCategoryUpstream {
				t.Fatalf("error = %v, want the delete-file task failure", err)
			}
			if err.Error() != "WPS operation failed: delete file task" {
				t.Fatalf("message = %q, want the operation-only text", err.Error())
			}
		})
	}
}

func TestWaitForTaskRejectsFailedResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"error"}`)},
	}}
	client := newWriteClient(t, opener, nil)

	err := client.WaitForTask(context.Background(), "task-uuid", "move file", 0, time.Second)
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "move file progress" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the move-file progress failure", err)
	}
}

func TestWaitForTaskTimesOut(t *testing.T) {
	opener := &progressOpener{status: 200, body: runningProgress()}
	client := newWriteClient(t, opener, nil)

	err := client.WaitForTask(context.Background(), "task-uuid", "move file", 0, 20*time.Millisecond)
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "move file task timeout" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the move-file task timeout", err)
	}
	if len(opener.requests) < 2 {
		t.Fatalf("polls = %d, want the loop to repoll until the deadline", len(opener.requests))
	}
}

func TestWaitForTaskStopsOnContextCancelDuringSleep(t *testing.T) {
	opener := &progressOpener{status: 200, body: runningProgress()}
	client := newWriteClient(t, opener, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := client.WaitForTask(ctx, "task-uuid", "move file", 50*time.Millisecond, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v to observe", elapsed)
	}
}

func TestWaitForTaskRejectsCanceledContextBeforeRequest(t *testing.T) {
	opener := &progressOpener{status: 200, body: runningProgress()}
	client := newWriteClient(t, opener, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.WaitForTask(ctx, "task-uuid", "move file", 0, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestWaitForTaskValidatesArguments(t *testing.T) {
	opener := &progressOpener{status: 200, body: runningProgress()}
	client := newWriteClient(t, opener, nil)

	cases := []struct {
		name     string
		interval time.Duration
		timeout  time.Duration
		message  string
	}{
		{"negative-interval", -time.Millisecond, time.Second, "poll_interval must not be negative"},
		{"zero-timeout", 0, 0, "poll_timeout must be positive"},
		{"negative-timeout", 0, -time.Second, "poll_timeout must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := client.WaitForTask(context.Background(), "task-uuid", "move file", tc.interval, tc.timeout)
			if err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestTaskPollDefaultsMatchPython(t *testing.T) {
	if DefaultTaskPollInterval != 500*time.Millisecond {
		t.Fatalf("interval default = %v, want 0.5s", DefaultTaskPollInterval)
	}
	if DefaultTaskPollTimeout != 60*time.Second {
		t.Fatalf("timeout default = %v, want 60s", DefaultTaskPollTimeout)
	}
}

func TestFinishEqualsOneMirrorsPythonComparison(t *testing.T) {
	cases := []struct {
		value any
		want  bool
	}{
		{json.Number("1"), true},
		{json.Number("1.0"), true},
		{true, true},
		{json.Number("0"), false},
		{json.Number("2"), false},
		{false, false},
		{json.Number("1.5"), false},
		{"1", false},
		{nil, false},
		{map[string]any{}, false},
	}
	for _, tc := range cases {
		if got := finishEqualsOne(tc.value); got != tc.want {
			t.Errorf("finishEqualsOne(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
