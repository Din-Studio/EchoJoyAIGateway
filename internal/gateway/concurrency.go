package gateway

import (
	"context"
	"net/http"

	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

var reasonConcurrencyLimit = reason{Status: http.StatusTooManyRequests, Code: "concurrency_limit_exceeded", Message: "Concurrency limit exceeded."}

// acquireRequestConcurrency takes the cluster-wide and access key slots of one
// logical request. Limits are read from the current snapshot, so a settings
// change applies to the next admission; held slots are never recounted. A
// missing key reports configuration_changed, a full scope reports
// concurrency_limit_exceeded, and an unavailable Redis returns the error.
func (handler *Handler) acquireRequestConcurrency(ctx context.Context, keyID uint) (release func(), failure *reason, err error) {
	snapshot := handler.manager.Current()
	if snapshot == nil {
		return nil, &reasonConfigurationChanged, nil
	}
	key, exists := snapshot.AccessKeysByID[keyID]
	if !exists {
		return nil, &reasonConfigurationChanged, nil
	}
	release, acquired, err := handler.concurrency.AcquireRequest(ctx, keyID, snapshot.Settings.GlobalConcurrencyLimit, key.ConcurrencyLimit)
	if err != nil {
		return nil, nil, err
	}
	if !acquired {
		return nil, &reasonConcurrencyLimit, nil
	}
	return release, nil, nil
}

// acquireGroupConcurrency takes one slot of the group for an upstream call.
func (handler *Handler) acquireGroupConcurrency(ctx context.Context, groupID uint) (release func(), failure *reason, err error) {
	snapshot := handler.manager.Current()
	if snapshot == nil {
		return nil, &reasonConfigurationChanged, nil
	}
	group, exists := snapshot.Groups[groupID]
	if !exists {
		return nil, &reasonConfigurationChanged, nil
	}
	release, acquired, err := handler.concurrency.AcquireGroup(ctx, groupID, group.ConcurrencyLimit)
	if err != nil {
		return nil, nil, err
	}
	if !acquired {
		return nil, &reasonConcurrencyLimit, nil
	}
	return release, nil, nil
}

// concurrencyControlRequest reports a Responses cancellation: it creates no
// content, so it neither takes a slot nor waits for one.
func concurrencyControlRequest(request *http.Request, route route) bool {
	if route.Protocol != protocol.OpenAIResponses {
		return false
	}
	metadata, err := dialect.NewOpenAIResponses().InspectRequest(&dialect.ParsedRequest{Method: request.Method, Path: request.URL.Path, RawQuery: request.URL.RawQuery})
	return err == nil && metadata.Operation == execution.OperationResponsesCancel
}
