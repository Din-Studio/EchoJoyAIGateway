package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/utils"
	"gpt-load/internal/protocol"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
)

func (handler *Handler) responseBindingObserver(
	ctx context.Context,
	accessKeyID uint,
	selection scheduler.Selection,
	ref state.CredentialRef,
	request *dialect.ParsedRequest,
	autoSelections ...*automodel.Selection,
) func([]byte) error {
	if request == nil || request.Method != http.MethodPost || request.Path != "/v1/responses" ||
		selection.RouteMode != execution.RouteNative || selection.ResponsesStoreDowngraded ||
		selection.ResolvedTarget.ResponsesStoreHandling(
			protocol.OpenAIResponses, execution.OperationResponsesCreate,
		) != channel.ResponsesStoreHandlingUpstreamManaged {
		return nil
	}
	var options struct {
		Store *bool `json:"store"`
	}
	if json.Unmarshal(request.Body, &options) != nil || (options.Store != nil && !*options.Store) {
		return nil
	}
	// Streams repeat the response object in several events; record each ID
	// once so shared stores are not called again for the same ownership.
	recordedID := ""
	return func(payload []byte) error {
		var response struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			Store  *bool  `json:"store"`
		}
		if json.Unmarshal(payload, &response) != nil || response.Object != "response" ||
			response.ID == "" || (response.Store != nil && !*response.Store) || response.ID == recordedID {
			return nil
		}
		recorded, err := handler.responseBindings.Record(ctx, accessKeyID, response.ID, ref, autoSelections...)
		if err != nil {
			// 保留原错误链，客户端取消仍可被 errors.Is(context.Canceled) 识别。
			return fmt.Errorf("%w: %w", errResponseOwnershipUnavailable, err)
		}
		if !recorded {
			return fmt.Errorf("%w: response ownership could not be recorded", ErrUpstreamProtocol)
		}
		recordedID = response.ID
		return nil
	}
}

// errResponseOwnershipUnavailable marks a response whose ownership could not
// reach the shared store. It is an internal failure: it must never be judged
// as an upstream fault against the credential that produced the response.
var errResponseOwnershipUnavailable = errors.New("response ownership store unavailable")

// responseOwnershipEvidence describes a response withheld because its
// ownership could not be recorded.
func responseOwnershipEvidence(err error) *execution.ErrorEvidence {
	code := "response_binding_conflict"
	if errors.Is(err, errResponseOwnershipUnavailable) {
		code = "response_binding_unavailable"
	}
	return &execution.ErrorEvidence{
		Kind: execution.ErrorKindInternal, OriginHint: execution.ErrorOriginInternal,
		ScopeHint: execution.ErrorScopeRequest, Code: code,
		Summary: "Response ownership could not be recorded.", ReplaySafety: execution.ReplaySafetyUnknown,
	}
}

// lookupResponseBinding resolves previous_response_id ownership. A store
// failure is reported as unavailable shared state rather than a missing
// binding, because the ownership may exist.
func (handler *Handler) lookupResponseBinding(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool, *reason) {
	binding, found, err := handler.responseBindings.Lookup(ctx, accessKeyID, responseID)
	if err != nil {
		utils.LogPlaneBestEffort(handler.logger, logrus.WarnLevel, utils.LogPlaneData,
			logrus.Fields{"event": "response_binding.lookup_unavailable", "error": err.Error()},
			"Shared response ownership is unavailable")
		return state.ResponseBinding{}, false, &reasonClusterStateUnavailable
	}
	return binding, found, nil
}
