package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
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
	auto *automodel.Selection,
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
	// A streaming attempt carries the same response object through several
	// events, so without this the identical ownership would be recorded three
	// or more times. Recording it once also keeps the only possible failure on
	// the first response event, before any byte has reached the client.
	var recorded string
	return func(payload []byte) error {
		var response struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			Store  *bool  `json:"store"`
		}
		if json.Unmarshal(payload, &response) != nil || response.Object != "response" ||
			response.ID == "" || (response.Store != nil && !*response.Store) {
			return nil
		}
		if response.ID == recorded {
			return nil
		}
		stored, err := handler.responseBindings.Record(ctx, accessKeyID, response.ID, ref, auto)
		if err != nil {
			return fmt.Errorf("%w: %w", errCoordinationUnavailable, err)
		}
		if !stored {
			return fmt.Errorf("%w: response ownership could not be recorded", ErrUpstreamProtocol)
		}
		recorded = response.ID
		return nil
	}
}
