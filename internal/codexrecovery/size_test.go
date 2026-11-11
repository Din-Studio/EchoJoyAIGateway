package codexrecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEncodedSizeMatchesJSON(t *testing.T) {
	text := "<>&\"\\\b\f\n\r\t\x00\u2028\u2029中文😀"
	for i := 0; i < 256; i++ {
		text += string(rune(i))
	}
	values := []any{nil, true, false, json.Number("9007199254740993"), text, "invalid\xff", []any{}, map[string]any{}, map[string]any{text: []any{nil, true, false, json.Number("-1.2e3"), text}}}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		size := int64(len(raw))
		if got := encodedSize(value, size); got != size {
			t.Fatalf("size=%d want=%d: %q", got, size, raw)
		}
		if got := encodedSize(value, size-1); got <= size-1 {
			t.Fatalf("oversize accepted=%d", got)
		}
	}
}

func TestRecoveryEncodingRespectsLimitAndPreservesInput(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_old","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"` + strings.Repeat("<", 500) + `"}]}]}`)
	original := bytes.Clone(body)
	operations := []struct {
		name string
		run  func(int64) ([]byte, Removals, bool, error)
	}{
		{"recovery", func(limit int64) ([]byte, Removals, bool, error) { return Sanitize(body, limit) }},
		{"cached_digest", func(limit int64) ([]byte, Removals, bool, error) {
			return StripEncryptedContentDigests(body, map[string]struct{}{EncryptedContentDigest("opaque"): {}}, limit)
		}},
		{"cached_id", func(limit int64) ([]byte, Removals, bool, error) {
			return StripHistoryItemIDs(body, map[string]struct{}{"rs_old": {}}, limit)
		}},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			output, _, ok, err := test.run(1024)
			if !errors.Is(err, ErrBodyTooLarge) || ok || output != nil {
				t.Fatalf("limit output=%d ok=%v err=%v", len(output), ok, err)
			}
			output, _, ok, err = test.run(1 << 20)
			if err != nil || !ok {
				t.Fatalf("recovery %v %v", ok, err)
			}
			exact, _, ok, err := test.run(int64(len(output)))
			if err != nil || !ok || !bytes.Equal(exact, output) {
				t.Fatal("exact limit rejected")
			}
			if !bytes.Equal(body, original) {
				t.Fatal("input modified")
			}
		})
	}
}
