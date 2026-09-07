// The async task poller ports client.py's _wait_for_task: one shared
// progress loop for move and delete with the captured success fields and
// operation-only errors. Go adds context cancellation: a canceled context
// stops the sleep immediately and aborts an in-flight poll request.

package wps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Task polling defaults mirror the move/delete keyword defaults
// (poll_interval=0.5, poll_timeout=60.0).
const (
	DefaultTaskPollInterval = 500 * time.Millisecond
	DefaultTaskPollTimeout  = 60 * time.Second
)

// WaitForTask polls the batch task progress endpoint until the task
// finishes successfully. The interval must not be negative and the timeout
// must be positive, matching the move/delete argument checks in Python.
func (c *Client) WaitForTask(
	ctx context.Context,
	taskUUID string,
	operation string,
	pollInterval time.Duration,
	pollTimeout time.Duration,
) error {
	if pollInterval < 0 {
		return errors.New("poll_interval must not be negative")
	}
	if pollTimeout <= 0 {
		return errors.New("poll_timeout must be positive")
	}
	deadline := time.Now().Add(pollTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := c.RequestJSONContext(ctx, JSONRequest{
			Path:  "/3rd/drive/api/v5/files/batch/task/progress",
			Query: []QueryPair{{Key: "taskuuid", Value: taskUUID}},
		})
		if err != nil {
			return err
		}
		if result, present := payload["result"]; present && result != nil && result != "ok" {
			return model.NewWpsAPIError(operation+" progress", 0, model.WpsCategoryUpstream)
		}
		status, _ := payload["status"].(string)
		if finishEqualsOne(payload["finish"]) || status == "success" {
			if failedListPresent(payload["failed_list"]) {
				return model.NewWpsAPIError(operation, http.StatusConflict, model.WpsCategoryUpstream)
			}
			return nil
		}
		if status == "failed" || status == "error" {
			return model.NewWpsAPIError(operation+" task", 0, model.WpsCategoryUpstream)
		}
		if !time.Now().Before(deadline) {
			return model.NewWpsAPIError(operation+" task timeout", 0, model.WpsCategoryUpstream)
		}
		if pollInterval > 0 {
			timer := time.NewTimer(pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// finishEqualsOne mirrors progress.get("finish") == 1: Python treats true
// and 1.0 as equal to 1, but the string "1" is not.
func finishEqualsOne(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed == 1
		}
		if parsed, err := typed.Float64(); err == nil {
			return parsed == 1
		}
		return false
	default:
		return false
	}
}

// failedListPresent mirrors failed_list not in (None, []): only a missing
// field, null, or an empty list counts as clean; any other value (including
// a non-list) fails the task with the 409 surface.
func failedListPresent(value any) bool {
	if value == nil {
		return false
	}
	if list, isList := value.([]any); isList {
		return len(list) > 0
	}
	return true
}
