package codexrecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// InvalidLineageKey scopes a session lineage to one credential identity. The
// same item can be valid for the account that created it and invalid for any
// other account, including another account in the same group.
func InvalidLineageKey(credentialID uint, identityGeneration uint64, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if credentialID == 0 || sessionKey == "" {
		return ""
	}
	return strconv.FormatUint(uint64(credentialID), 10) + ":" +
		strconv.FormatUint(identityGeneration, 10) + "\x00" + sessionKey
}

// EncryptedContentDigest returns the stable SHA-256 digest of one opaque value.
func EncryptedContentDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// EncryptedContentDigests collects every opaque encrypted_content value carried
// by a Responses input. Only digests are retained by InvalidLineage.
func EncryptedContentDigests(body []byte) []string {
	object, ok := decodeJSONObject(body)
	if !ok {
		return nil
	}
	items, _ := responseInputItems(object["input"])
	if items == nil {
		return nil
	}
	seen := make(map[string]struct{})
	digests := make([]string, 0, 4)
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if encrypted, isString := typed["encrypted_content"].(string); isString && encrypted != "" {
				digest := EncryptedContentDigest(encrypted)
				if _, duplicate := seen[digest]; !duplicate {
					seen[digest] = struct{}{}
					digests = append(digests, digest)
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(items)
	return digests
}

// HistoryItemIDs collects IDs from replayable Responses input items. Only these
// IDs can be removed safely before a later attempt.
func HistoryItemIDs(body []byte) []string {
	object, ok := decodeJSONObject(body)
	if !ok {
		return nil
	}
	items, _ := responseInputItems(object["input"])
	if items == nil {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0, 4)
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if !replayableHistoryItem(record) {
			continue
		}
		id, _ := record["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
