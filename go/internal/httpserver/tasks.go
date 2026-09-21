package httpserver

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/tasks"
)

type boundBatchStorage interface {
	ApplyBoundBatch(context.Context, string, string, string, string, string) error
}

func (d *RESTDispatcher) taskMetadata(path string) (model.RemoteEntry, error) {
	if source, ok := d.read.(interface {
		BatchMetadata(string) (model.RemoteEntry, error)
	}); ok {
		return source.BatchMetadata(path)
	}
	return d.read.Metadata(path)
}

// EnableTasks initializes the durable queue only when safe ID-bound mutation
// support and an account/workspace identity source are available.
func (d *RESTDispatcher) EnableTasks(file string, identity func() ([32]byte, error)) error {
	if _, ok := d.mutations.(boundBatchStorage); !ok || identity == nil {
		return errChainConfig("task storage must support identity-bound operations")
	}
	d.taskIdentity = identity
	manager, err := tasks.New(tasks.Config{File: file, Validate: d.validateTaskIdentity, Execute: d.executeTaskItem})
	if err != nil {
		return err
	}
	d.tasks = manager
	return nil
}
func (d *RESTDispatcher) CloseTasks() {
	if d.tasks != nil {
		d.tasks.Close()
	}
}
func (d *RESTDispatcher) validateTaskIdentity(spec tasks.Spec) error {
	identity, err := d.taskIdentity()
	if err != nil {
		return model.NewStorageError(model.KindIOFailure, "task identity unavailable")
	}
	if hex.EncodeToString(identity[:]) != spec.Identity {
		return model.NewStorageError(model.KindAlreadyExists, "account or storage configuration changed; create a new task after verifying the selected files")
	}
	return nil
}

func (d *RESTDispatcher) serveTasks(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if d.tasks == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "background tasks are unavailable")
	}
	parts := strings.Split(route.Suffix, "/")
	if len(parts) > 3 {
		return model.NewStorageError(model.KindEntryNotFound, "unknown task route")
	}
	if r.Method == http.MethodGet {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		if len(parts) == 1 {
			list, failed := d.tasks.List()
			return sendJSON(w, r, http.StatusOK, map[string]any{"tasks": list, "persistence_error": failed}, d.limits, nil)
		}
		if len(parts) == 2 {
			task, err := d.tasks.Get(parts[1])
			if err != nil {
				return mapTaskError(err)
			}
			return sendJSON(w, r, http.StatusOK, map[string]any{"task": task}, d.limits, nil)
		}
	}
	if r.Method == http.MethodPost {
		if len(parts) == 1 {
			spec, err := d.taskSpec(w, r)
			if err != nil || spec == nil {
				return err
			}
			task, err := d.tasks.Submit(*spec)
			if err != nil {
				return mapTaskError(err)
			}
			return sendJSON(w, r, http.StatusAccepted, map[string]any{"task": task}, d.limits, nil)
		}
		if len(parts) == 3 {
			if err := discardBody(w, r, d.limits); err != nil {
				return err
			}
			var task tasks.Task
			var err error
			switch parts[2] {
			case "cancel":
				task, err = d.tasks.Cancel(parts[1])
			case "retry":
				task, err = d.tasks.Retry(parts[1])
			default:
				return model.NewStorageError(model.KindEntryNotFound, "unknown task route")
			}
			if err != nil {
				return mapTaskError(err)
			}
			status := http.StatusOK
			if parts[2] == "retry" {
				status = http.StatusAccepted
			}
			return sendJSON(w, r, status, map[string]any{"task": task}, d.limits, nil)
		}
	}
	return model.NewStorageError(model.KindBadRequest, "unsupported task method")
}

func (d *RESTDispatcher) taskSpec(w http.ResponseWriter, r *http.Request) (*tasks.Spec, error) {
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return nil, err
	}
	operation, _ := payload["operation"].(string)
	if operation != "copy" && operation != "move" && operation != "delete" {
		return nil, errBadRequest("operation must be copy, move or delete")
	}
	for key := range payload {
		if key != "operation" && key != "paths" && (key != "destination" || operation == "delete") {
			return nil, errBadRequest("unknown task field")
		}
	}
	paths, err := selectionPaths(payload["paths"])
	if err != nil {
		return nil, err
	}
	identity, err := d.taskIdentity()
	if err != nil {
		return nil, model.NewStorageError(model.KindIOFailure, "task identity unavailable")
	}
	spec := &tasks.Spec{Operation: operation, Identity: hex.EncodeToString(identity[:]), Sources: make([]tasks.Binding, 0, len(paths))}
	if cache, ok := d.read.(interface{ InvalidateMetadataCache() }); ok {
		cache.InvalidateMetadataCache()
	}
	if operation != "delete" {
		destination, _ := payload["destination"].(string)
		destination, err = canonicalRemotePath(destination)
		if err != nil {
			return nil, err
		}
		if len(destination) > tasks.MaxPathBytes {
			return nil, errBadRequest("task path exceeds the size limit")
		}
		entry, err := d.taskMetadata(destination)
		if err != nil {
			return nil, err
		}
		if entry.Kind != model.KindFolder {
			return nil, model.NewStorageError(model.KindNotFolder, "destination is not a folder")
		}
		spec.Destination, spec.DestinationID = destination, entry.ID
	}
	names := make(map[string]bool)
	total := len(spec.Destination)
	for _, source := range paths {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if len(source) > tasks.MaxPathBytes {
			return nil, errBadRequest("task path exceeds the size limit")
		}
		if spec.Destination != "" {
			name := path.Base(source)
			if names[name] {
				return nil, errBadRequest("selected entries have duplicate destination names")
			}
			names[name] = true
			if spec.Destination == source || strings.HasPrefix(spec.Destination, source+"/") {
				return nil, errBadRequest("destination must not be inside a selected entry")
			}
		}
		entry, err := d.taskMetadata(source)
		if err != nil {
			return nil, err
		}
		if entry.ID == "" {
			return nil, model.NewStorageError(model.KindUnsupportedOperation, "source cannot be bound to a task")
		}
		total += len(source) + len(entry.ID)
		if total > tasks.MaxTaskPathBytes {
			return nil, errBadRequest("task paths exceed the total size limit")
		}
		spec.Sources = append(spec.Sources, tasks.Binding{Path: source, ID: entry.ID})
	}
	if err := d.validateTaskIdentity(*spec); err != nil {
		return nil, err
	}
	return spec, nil
}

func (d *RESTDispatcher) executeTaskItem(ctx context.Context, spec tasks.Spec, source tasks.Binding) tasks.Result {
	if err := d.validateTaskIdentity(spec); err != nil {
		return taskResult(err)
	}
	if err := ctx.Err(); err != nil {
		return tasks.Result{Status: 409, Error: "task stopped before the item started"}
	}
	destination := ""
	if spec.Destination != "" {
		destination = strings.TrimSuffix(spec.Destination, "/") + "/" + path.Base(source.Path)
	}
	// Browser task creation never captures lock tokens: a later operation must
	// respect the locks that exist when it actually starts.
	if !d.locks.allowsTree(source.Path, nil) || (destination != "" && (!d.locks.Allows(spec.Destination, nil) || !d.locks.allowsTree(destination, nil))) {
		return tasks.Result{Status: 423, Error: "resource is locked", SafeToRetry: true}
	}
	d.invalidateSearch()
	defer d.invalidateSearch()
	err := d.mutations.(boundBatchStorage).ApplyBoundBatch(ctx, spec.Operation, source.Path, destination, source.ID, spec.DestinationID)
	if err != nil {
		result := taskResult(err)
		// A transport failure after a remote mutation may have taken effect.
		// Only explicit local rejections are eligible for an automated retry.
		if domain, ok := model.AsStorageError(err); ok {
			result.SafeToRetry = domain.Kind == model.KindAlreadyExists || domain.Kind == model.KindUnsupportedOperation || domain.Kind == model.KindInvalidPath || domain.Kind == model.KindNotFolder || domain.Kind == model.KindEntryNotFound
		}
		if !result.SafeToRetry {
			result.Error += "; result may be incomplete, verify remote files before creating new work"
		}
		return result
	}
	return tasks.Result{Status: 200}
}
func taskResult(err error) tasks.Result {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return tasks.Result{Status: 409, Error: "task operation stopped; verify remote changes before retrying"}
	}
	result := batchFailure(&http.Request{Method: http.MethodPost}, "", err)
	return tasks.Result{Status: result.Status, Error: result.Error}
}
func mapTaskError(err error) error {
	switch {
	case errors.Is(err, tasks.ErrBusy):
		return model.NewStorageError(model.KindServiceBusy, err.Error())
	case errors.Is(err, tasks.ErrUnavailable):
		return model.NewStorageError(model.KindIOFailure, err.Error())
	case errors.Is(err, tasks.ErrNotFound):
		return model.NewStorageError(model.KindEntryNotFound, err.Error())
	case errors.Is(err, tasks.ErrNotRetryable):
		return model.NewStorageError(model.KindAlreadyExists, err.Error())
	case errors.Is(err, tasks.ErrInvalid):
		return errBadRequest(err.Error())
	}
	return err
}
