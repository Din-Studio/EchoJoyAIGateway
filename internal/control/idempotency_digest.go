package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gpt-load/internal/platform/canonicaljson"
	app_errors "gpt-load/internal/platform/errors"
)

const (
	idempotencyDigestDomain = "gpt-load/control-idempotency"
	idempotencyAuthScopeID  = "control-admin-v1"
)

type operationKind string

const (
	operationKindAccessKeyCreate  operationKind = "access_key_create"
	operationKindAccessKeyRotate  operationKind = "access_key_rotate"
	operationKindAccessKeyUpdate  operationKind = "access_key_update"
	operationKindGroupCreate      operationKind = "group_create"
	operationKindCredentialImport operationKind = "credential_import"
)

type idempotencyDigestInput struct {
	Version         uint
	Method          string
	OperationKind   operationKind
	PathTemplate    string
	ResourceLocator string
	AuthScopeID     string
	CanonicalBody   []byte
}

type idempotencyDigestResult struct {
	Digest      [sha256.Size]byte
	FramedInput []byte
}

func buildIdempotencyDigest(
	input idempotencyDigestInput,
) (idempotencyDigestResult, error) {
	if input.Version != 1 {
		return idempotencyDigestResult{}, fmt.Errorf(
			"unsupported idempotency digest version %d: %w",
			input.Version,
			app_errors.ErrInternalServer,
		)
	}
	if input.Method == "" || input.Method != strings.ToUpper(input.Method) {
		return idempotencyDigestResult{}, fmt.Errorf("idempotency method must be uppercase")
	}
	if !input.OperationKind.valid() ||
		input.PathTemplate == "" ||
		input.ResourceLocator == "" ||
		input.AuthScopeID == "" ||
		len(input.CanonicalBody) == 0 {
		return idempotencyDigestResult{}, fmt.Errorf("idempotency digest fields are incomplete")
	}
	if !utf8.Valid(input.CanonicalBody) {
		return idempotencyDigestResult{}, fmt.Errorf("canonical body is not valid UTF-8")
	}
	canonicalBody, err := canonicaljson.Canonicalize(input.CanonicalBody)
	if err != nil {
		return idempotencyDigestResult{}, fmt.Errorf("validate canonical body: %w", err)
	}
	if !bytes.Equal(canonicalBody, input.CanonicalBody) {
		return idempotencyDigestResult{}, fmt.Errorf("idempotency body is not canonical JSON")
	}

	fields := [][]byte{
		[]byte(strconv.FormatUint(uint64(input.Version), 10)),
		[]byte(input.Method),
		[]byte(input.OperationKind),
		[]byte(input.PathTemplate),
		[]byte(input.ResourceLocator),
		[]byte(input.AuthScopeID),
		input.CanonicalBody,
	}
	var framed bytes.Buffer
	framed.WriteString(idempotencyDigestDomain)
	framed.WriteByte(0)
	for _, field := range fields {
		if len(field) > math.MaxUint32 {
			return idempotencyDigestResult{}, fmt.Errorf("idempotency digest field is too large")
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		framed.Write(length[:])
		framed.Write(field)
	}
	inputBytes := framed.Bytes()
	return idempotencyDigestResult{
		Digest:      sha256.Sum256(inputBytes),
		FramedInput: append([]byte(nil), inputBytes...),
	}, nil
}

func (kind operationKind) valid() bool {
	switch kind {
	case operationKindAccessKeyCreate, operationKindAccessKeyRotate, operationKindAccessKeyUpdate, operationKindGroupCreate,
		operationKindCredentialImport:
		return true
	default:
		return false
	}
}

func canonicalIdempotencyBody(value any) ([]byte, error) {
	body, err := canonicaljson.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode idempotency body: %w", err)
	}
	return body, nil
}

// credentialEntriesDigest makes the digest independent of row order and of
// whitespace the credential normalizer would trim anyway.
func credentialEntriesDigest(entries []CredentialEntryInput) ([]json.RawMessage, error) {
	if len(entries) == 0 || len(entries) > maxCredentialEntries {
		return nil, app_errors.ErrValidation
	}
	digest := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		encoded, err := canonicaljson.Marshal(CredentialEntryInput{
			Fields:           trimmedCredentialFields(entry.Fields),
			ConcurrencyLimit: entry.ConcurrencyLimit,
		})
		if err != nil {
			return nil, app_errors.ErrInternalServer
		}
		digest = append(digest, encoded)
	}
	sort.Slice(digest, func(left, right int) bool {
		return bytes.Compare(digest[left], digest[right]) < 0
	})
	return digest, nil
}
