package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

const (
	cacheDocumentVersion  = 1
	maxCacheDocumentBytes = maxCatalogBodyBytes + 1024*1024
)

var cacheEnvelopeCanonicalFields = []string{
	"version",
	"etag",
	"last_modified",
	"checked_at_ms",
	"successful_fetch_at_ms",
	"raw",
}

type cacheDocument struct {
	Version                 int             `json:"version"`
	ETag                    string          `json:"etag,omitempty"`
	LastModified            string          `json:"last_modified,omitempty"`
	CheckedAtMillis         int64           `json:"checked_at_ms,omitempty"`
	SuccessfulFetchAtMillis int64           `json:"successful_fetch_at_ms"`
	Raw                     json.RawMessage `json:"raw"`
}

// DecodeCache fully revalidates an encoded last-known-good document, the same
// bytes EncodeCache produces.
func DecodeCache(contents []byte) (CachedCatalog, error) {
	if int64(len(contents)) > maxCacheDocumentBytes {
		return CachedCatalog{}, fmt.Errorf("catalog cache exceeds size limit")
	}
	if _, err := decodeCanonicalObject(contents, "catalog cache envelope", cacheEnvelopeCanonicalFields); err != nil {
		return CachedCatalog{}, fmt.Errorf("validate catalog cache envelope: %w", err)
	}
	var document cacheDocument
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return CachedCatalog{}, fmt.Errorf("decode catalog cache: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return CachedCatalog{}, err
	}
	if document.Version != cacheDocumentVersion {
		return CachedCatalog{}, fmt.Errorf("unsupported catalog cache version %d", document.Version)
	}
	metadata := Metadata{
		ETag:                    document.ETag,
		LastModified:            document.LastModified,
		CheckedAtMillis:         document.CheckedAtMillis,
		SuccessfulFetchAtMillis: document.SuccessfulFetchAtMillis,
	}
	if err := validateMetadata(metadata, true); err != nil {
		return CachedCatalog{}, err
	}
	if len(document.Raw) == 0 || int64(len(document.Raw)) > maxCatalogBodyBytes {
		return CachedCatalog{}, fmt.Errorf("catalog cache raw document is missing or oversized")
	}
	modelsDevSnapshot, err := Parse(bytes.NewReader(document.Raw))
	if err != nil {
		return CachedCatalog{}, fmt.Errorf("parse cached Models.dev catalog: %w", err)
	}
	snapshot, err := MergeOfficial(modelsDevSnapshot)
	if err != nil {
		return CachedCatalog{}, err
	}
	return CachedCatalog{
		Metadata: metadata,
		RawJSON:  append(json.RawMessage(nil), document.Raw...),
		Snapshot: snapshot,
	}, nil
}

// EncodeCache encodes a fully validated and internally consistent 200 response
// as a last-known-good document that DecodeCache accepts.
func EncodeCache(result SyncResult) ([]byte, error) {
	document, err := cacheDocumentForResult(result)
	if err != nil {
		return nil, err
	}
	contents, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode catalog cache: %w", err)
	}
	contents = append(contents, '\n')
	if int64(len(contents)) > maxCacheDocumentBytes {
		return nil, fmt.Errorf("encoded catalog cache exceeds size limit")
	}
	return contents, nil
}

func cacheDocumentForResult(result SyncResult) (cacheDocument, error) {
	if result.NotModified || len(result.RawJSON) == 0 || result.Snapshot == nil {
		return cacheDocument{}, fmt.Errorf("catalog cache requires a validated 200 result")
	}
	if int64(len(result.RawJSON)) > maxCatalogBodyBytes {
		return cacheDocument{}, fmt.Errorf("catalog raw document exceeds 32 MiB limit")
	}
	if err := validateMetadata(result.Metadata, true); err != nil {
		return cacheDocument{}, err
	}
	modelsDevSnapshot, err := Parse(bytes.NewReader(result.RawJSON))
	if err != nil {
		return cacheDocument{}, fmt.Errorf("revalidate Models.dev catalog: %w", err)
	}
	reparsed, err := MergeOfficial(modelsDevSnapshot)
	if err != nil {
		return cacheDocument{}, err
	}
	if !reflect.DeepEqual(reparsed, result.Snapshot) {
		return cacheDocument{}, fmt.Errorf("catalog raw document and snapshot are inconsistent")
	}
	return cacheDocument{
		Version:                 cacheDocumentVersion,
		ETag:                    result.Metadata.ETag,
		LastModified:            result.Metadata.LastModified,
		CheckedAtMillis:         result.Metadata.CheckedAtMillis,
		SuccessfulFetchAtMillis: result.Metadata.SuccessfulFetchAtMillis,
		Raw:                     append(json.RawMessage(nil), result.RawJSON...),
	}, nil
}
