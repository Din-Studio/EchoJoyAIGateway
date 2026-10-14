package bifrost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gpt-load/internal/channel"
	"gpt-load/internal/channel/spec"
	"gpt-load/internal/execution"
)

// responseScrubber removes one channel's declared platform metadata from
// native responses. A nil scrubber removes nothing.
type responseScrubber struct {
	anyKeys        map[string]struct{}
	usageKeys      map[string]struct{}
	markers        [][]byte
	headerPrefixes []string
}

func (r *Runtime) responseScrubber(spec execution.AttemptSpec) *responseScrubber {
	if r == nil || r.registry == nil {
		return nil
	}
	rules, _ := r.registry.ResponseScrub(channel.ID(spec.ChannelID))
	return newResponseScrubber(rules)
}

func newResponseScrubber(rules spec.ResponseScrub) *responseScrubber {
	if rules.Empty() {
		return nil
	}
	scrubber := &responseScrubber{
		anyKeys:   make(map[string]struct{}, len(rules.AnyKeys)),
		usageKeys: make(map[string]struct{}, len(rules.UsageKeys)),
	}
	for _, key := range rules.AnyKeys {
		scrubber.anyKeys[key] = struct{}{}
		scrubber.markers = append(scrubber.markers, []byte(`"`+key+`"`))
	}
	for _, key := range rules.UsageKeys {
		scrubber.usageKeys[key] = struct{}{}
		scrubber.markers = append(scrubber.markers, []byte(`"`+key+`"`))
	}
	for _, prefix := range rules.HeaderPrefixes {
		scrubber.headerPrefixes = append(scrubber.headerPrefixes, strings.ToLower(prefix))
	}
	return scrubber
}

// scrubHeaders deletes declared response headers in place.
func (s *responseScrubber) scrubHeaders(headers http.Header) {
	if s == nil {
		return
	}
	for name := range headers {
		lowered := strings.ToLower(name)
		for _, prefix := range s.headerPrefixes {
			if strings.HasPrefix(lowered, prefix) {
				delete(headers, name)
				break
			}
		}
	}
}

// scrubJSON returns body unchanged unless it contains a declared key.
func (s *responseScrubber) scrubJSON(body []byte) ([]byte, error) {
	if s == nil || !s.mayContainKey(body) {
		return body, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode response for metadata scrub: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("decode response for metadata scrub: trailing data")
	}
	if !s.scrubValue(value, false) {
		return body, nil
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode scrubbed response: %w", err)
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func (s *responseScrubber) mayContainKey(body []byte) bool {
	for _, marker := range s.markers {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func (s *responseScrubber) scrubValue(value any, usage bool) bool {
	removed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			_, anyKey := s.anyKeys[key]
			_, usageKey := s.usageKeys[key]
			if anyKey || usage && usageKey {
				delete(typed, key)
				removed = true
				continue
			}
			removed = s.scrubValue(child, key == "usage") || removed
		}
	case []any:
		for _, child := range typed {
			removed = s.scrubValue(child, false) || removed
		}
	}
	return removed
}
