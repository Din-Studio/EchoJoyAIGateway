package gateway

import (
	"context"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/affinity"
	"gpt-load/internal/dialect"
	"gpt-load/internal/protocol"
)

// InvalidLineageStore holds rejected continuation evidence shared by replicas.
type InvalidLineageStore interface {
	Lookup(context.Context, string) (map[string]struct{}, map[string]struct{}, error)
	Mark(context.Context, string, []string, []string) (int, error)
}

// invalidLineageKey derives the same tenant- and protocol-scoped session
// identity used for affinity. Explicit client signals win; the prompt prefix
// is the stable fallback when a client sends full growing history.
func (handler *Handler) invalidLineageKey(
	accessKeyID uint,
	clientProtocol protocol.Protocol,
	metadata dialect.RequestMetadata,
) string {
	if accessKeyID == 0 || clientProtocol != protocol.OpenAIResponses {
		return ""
	}
	var key affinity.Key
	switch {
	case metadata.PromptCacheKey != "":
		key = affinity.DerivePromptCacheKey(handler.encryption, accessKeyID, clientProtocol, metadata.PromptCacheKey)
	case metadata.PreviousResponseID != "":
		key = affinity.DeriveKey(
			handler.encryption,
			accessKeyID,
			clientProtocol,
			[]byte("codex-invalid-lineage\x00previous-response\x00"+metadata.PreviousResponseID),
		)
	case len(metadata.AffinityPrefix) > 0:
		key = affinity.DeriveKey(handler.encryption, accessKeyID, clientProtocol, metadata.AffinityPrefix)
	}
	if !key.Valid() {
		return ""
	}
	return string(key)
}

func (handler *Handler) logLineageFailure(err error) {
	if err != nil {
		handler.logger.WithError(err).WithFields(logrus.Fields{"event": "response_lineage.unavailable"}).Warn("Continuing without shared rejection evidence")
	}
}
