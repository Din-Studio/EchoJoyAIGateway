package bifrost

import (
	"bytes"
	"net/http"
	"testing"

	"gpt-load/internal/channel/spec"
)

func TestResponseScrubberRemovesDeclaredKeys(t *testing.T) {
	t.Parallel()

	scrubber := newResponseScrubber(spec.ResponseScrub{
		AnyKeys:   []string{"provider_metadata"},
		UsageKeys: []string{"cost"},
	})
	body := []byte(`{"choices":[{"message":{"content":"<b>","provider_metadata":{"gateway":{}}}}],` +
		`"content":[{"type":"tool_use","input":{"cost":5}}],"usage":{"prompt_tokens":2,"cost":0.0000336}}`)
	scrubbed, err := scrubber.scrubJSON(body)
	if err != nil {
		t.Fatalf("scrubJSON() error = %v", err)
	}
	want := `{"choices":[{"message":{"content":"<b>"}}],"content":[{"input":{"cost":5},"type":"tool_use"}],"usage":{"prompt_tokens":2}}`
	if string(scrubbed) != want {
		t.Fatalf("scrubJSON() = %s, want %s", scrubbed, want)
	}
}

func TestResponseScrubberKeepsBodiesWithoutDeclaredKeys(t *testing.T) {
	t.Parallel()

	scrubber := newResponseScrubber(spec.ResponseScrub{AnyKeys: []string{"provider_metadata"}, UsageKeys: []string{"cost"}})
	for _, body := range []string{
		`{"b":1,  "a":0.10}`,
		`{"cost":1,"usage":{"prompt_tokens":1}}`,
	} {
		scrubbed, err := scrubber.scrubJSON([]byte(body))
		if err != nil || !bytes.Equal(scrubbed, []byte(body)) {
			t.Fatalf("scrubJSON(%s) = %s, %v", body, scrubbed, err)
		}
	}
}

func TestResponseScrubberRemovesHeaderPrefixes(t *testing.T) {
	t.Parallel()

	scrubber := newResponseScrubber(spec.ResponseScrub{HeaderPrefixes: []string{"X-Vercel-", "X-Matched-Path"}})
	headers := http.Header{
		"X-Vercel-Id":    {"sin1::fra1"},
		"X-Vercel-Cache": {"MISS"},
		"X-Matched-Path": {"/v1/chat/completions"},
		"X-Request-Id":   {"req_1"},
	}
	scrubber.scrubHeaders(headers)
	if len(headers) != 1 || headers.Get("X-Request-Id") != "req_1" {
		t.Fatalf("scrubbed headers = %#v", headers)
	}
}

func TestEmptyResponseScrubDisablesScrubber(t *testing.T) {
	t.Parallel()

	if scrubber := newResponseScrubber(spec.ResponseScrub{}); scrubber != nil {
		t.Fatal("empty declaration enabled response scrubbing")
	}
}
