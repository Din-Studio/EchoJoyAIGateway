package catalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCacheRoundTripEmbedsRawJSONValueAndReparsesSnapshot(t *testing.T) {
	result := validSyncResult(t)
	contents, err := EncodeCache(result)
	if err != nil {
		t.Fatalf("EncodeCache() error = %v", err)
	}
	if bytes.Contains(contents, []byte(`"raw":"`)) || !bytes.Contains(contents, []byte(`"raw":{`)) {
		t.Fatalf("cache raw payload is not an embedded JSON value: %s", contents)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("cache document JSON error = %v", err)
	}
	if string(document["version"]) != "1" {
		t.Fatalf("cache version = %s, want 1", document["version"])
	}

	loaded, err := DecodeCache(contents)
	if err != nil {
		t.Fatalf("DecodeCache() error = %v", err)
	}
	if loaded.Metadata != result.Metadata || string(loaded.RawJSON) != string(result.RawJSON) || !reflect.DeepEqual(loaded.Snapshot, result.Snapshot) {
		t.Fatalf("decoded cache = %#v, want result %#v", loaded, result)
	}
	loadedProvider := loaded.Snapshot.Providers["openai"]
	loadedModel := loadedProvider.Models["gpt-x"]
	*loadedModel.Metadata.Capabilities.Attachment = false
	loadedModel.Metadata.Modalities.Input[0] = "audio"
	*loadedModel.Metadata.Limits.Context = 99
	loadedProvider.Models["gpt-x"] = loadedModel
	loaded.Snapshot.Providers["openai"] = loadedProvider
	sourceMetadata := result.Snapshot.Providers["openai"].Models["gpt-x"].Metadata
	if sourceMetadata.Capabilities.Attachment == nil || !*sourceMetadata.Capabilities.Attachment ||
		sourceMetadata.Modalities.Input[0] != "text" ||
		sourceMetadata.Limits.Context == nil || *sourceMetadata.Limits.Context != 1_000_000 {
		t.Fatalf("decoded cache shares snapshot metadata with source: %#v", sourceMetadata)
	}
	reloaded, err := DecodeCache(contents)
	if err != nil {
		t.Fatalf("DecodeCache() after caller mutation error = %v", err)
	}
	reloadedMetadata := reloaded.Snapshot.Providers["openai"].Models["gpt-x"].Metadata
	if reloadedMetadata.Capabilities.Attachment == nil || !*reloadedMetadata.Capabilities.Attachment ||
		reloadedMetadata.Modalities.Input[0] != "text" ||
		reloadedMetadata.Limits.Context == nil || *reloadedMetadata.Limits.Context != 1_000_000 {
		t.Fatalf("caller mutation changed cached metadata: %#v", reloadedMetadata)
	}
}

func TestDecodeCacheRejectsUnsupportedCorruptAndOversizeDocuments(t *testing.T) {
	validRaw := `{"openai":{"id":"openai","name":"OpenAI","models":{}}}`
	tests := []struct {
		name     string
		contents string
	}{
		{name: "duplicate version", contents: `{"version":2,"version":1,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "duplicate raw", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":{},"raw":` + validRaw + `}`},
		{name: "duplicate etag", contents: `{"version":1,"etag":"old","etag":"new","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "duplicate last modified", contents: `{"version":1,"last_modified":"old","last_modified":"new","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "duplicate checked timestamp", contents: `{"version":1,"checked_at_ms":2,"checked_at_ms":3,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "duplicate successful timestamp", contents: `{"version":1,"successful_fetch_at_ms":0,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "version case alias", contents: `{"version":2,"Version":1,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "raw case alias", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":{},"Raw":` + validRaw + `}`},
		{name: "etag case alias", contents: `{"version":1,"etag":"old","ETag":"new","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "last modified case alias", contents: `{"version":1,"last_modified":"old","Last_Modified":"new","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "checked timestamp case alias", contents: `{"version":1,"checked_at_ms":2,"Checked_At_MS":3,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "successful timestamp case alias", contents: `{"version":1,"successful_fetch_at_ms":0,"Successful_Fetch_At_MS":1,"raw":` + validRaw + `}`},
		{name: "unsupported version", contents: `{"version":2,"successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "zero successful fetch time", contents: `{"version":1,"successful_fetch_at_ms":0,"raw":` + validRaw + `}`},
		{name: "negative successful fetch time", contents: `{"version":1,"successful_fetch_at_ms":-1,"raw":` + validRaw + `}`},
		{name: "invalid etag control", contents: `{"version":1,"etag":"bad\nvalue","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "invalid last modified control", contents: `{"version":1,"last_modified":"bad\nvalue","successful_fetch_at_ms":1,"raw":` + validRaw + `}`},
		{name: "missing raw", contents: `{"version":1,"successful_fetch_at_ms":1}`},
		{name: "raw string instead object", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":"` + strings.ReplaceAll(validRaw, `"`, `\"`) + `"}`},
		{name: "invalid raw snapshot", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":{"openai":{"id":"wrong","name":"OpenAI","models":{}}}}`},
		{name: "duplicate raw provider", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":{"openai":{"id":"openai","name":"OpenAI","models":{}},"openai":{"id":"openai","name":"Other","models":{}}}}`},
		{name: "trailing cache JSON", contents: `{"version":1,"successful_fetch_at_ms":1,"raw":` + validRaw + `}{}`},
		{name: "malformed cache JSON", contents: `{"version":1`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeCache([]byte(test.contents)); err == nil {
				t.Fatalf("DecodeCache() accepted invalid document: %s", test.contents)
			}
		})
	}

	t.Run("oversize document", func(t *testing.T) {
		if _, err := DecodeCache(make([]byte, maxCacheDocumentBytes+1)); err == nil {
			t.Fatal("DecodeCache() accepted oversize document")
		}
	})
}

func TestEncodeCacheAcceptsOnlyInternallyConsistentValidated200Result(t *testing.T) {
	valid := validSyncResult(t)
	tests := []struct {
		name   string
		mutate func(*SyncResult)
	}{
		{name: "not modified", mutate: func(result *SyncResult) { result.NotModified = true }},
		{name: "missing raw", mutate: func(result *SyncResult) { result.RawJSON = nil }},
		{name: "missing snapshot", mutate: func(result *SyncResult) { result.Snapshot = nil }},
		{name: "invalid raw", mutate: func(result *SyncResult) { result.RawJSON = json.RawMessage(`{}` + `{}`) }},
		{name: "snapshot mismatch", mutate: func(result *SyncResult) {
			result.Snapshot = &Snapshot{Providers: map[string]Provider{}}
		}},
		{name: "missing successful fetch time", mutate: func(result *SyncResult) {
			result.Metadata.SuccessfulFetchAtMillis = 0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.mutate(&result)
			if _, err := EncodeCache(result); err == nil {
				t.Fatal("EncodeCache() accepted invalid result")
			}
		})
	}
}

func validSyncResult(t *testing.T) SyncResult {
	t.Helper()
	raw := json.RawMessage(`{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-x":{"id":"gpt-x","name":"GPT X","description":"General model","attachment":true,"modalities":{"input":["text","image"],"output":["text"]},"limit":{"context":1000000,"output":100000},"cost":{"input":0.000000001}}}}}`)
	snapshot, err := MergeOfficial(mustParse(t, string(raw)))
	if err != nil {
		t.Fatalf("MergeOfficial() error = %v", err)
	}
	return SyncResult{
		Metadata: Metadata{
			ETag:                    `"catalog-v1"`,
			LastModified:            "Mon, 03 Aug 2026 01:00:00 GMT",
			CheckedAtMillis:         1_754_180_400_123,
			SuccessfulFetchAtMillis: 1_754_180_400_123,
		},
		RawJSON:  raw,
		Snapshot: snapshot,
	}
}
