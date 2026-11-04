package clientcatalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpt-load/internal/catalog"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

func TestCatalogBudgetDoesNotSkipOversizedMiddleEntry(t *testing.T) {
	for _, version := range []string{"0.159.2", "0.154.0"} {
		t.Run(version, func(t *testing.T) {
			snapshot := &state.ConfigSnapshot{
				ExecutionCandidates:  state.ExecutionCandidateIndex{protocol.OpenAIResponses: {execution.OperationResponsesCreate: {}}},
				ClientModelOverrides: map[string]catalog.ClientModelOverrides{},
			}
			for index, name := range []string{"gpt-6-prefix-a", "gpt-6-prefix-b", "gpt-6-prefix-c"} {
				description := ""
				if index == 1 {
					description = strings.Repeat("x", 4096)
				}
				snapshot.ExecutionCandidates[protocol.OpenAIResponses][execution.OperationResponsesCreate][name] = []state.RouteTarget{{GroupID: 1}}
				snapshot.ClientModelOverrides[name] = catalog.ClientModelOverrides{Description: &description}
			}
			full, err := Build(snapshot, state.AccessKeyView{}, version, LimitBytes)
			if err != nil || full.IncludedCount != 3 {
				t.Fatalf("full catalog count=%d, err=%v", full.IncludedCount, err)
			}
			// The first and third entries fit together, but the middle one does
			// not fit after the first. A prefix must not fill that gap with #3.
			limit := int64(13) + full.Entries[0].Bytes + 1 + full.Entries[2].Bytes
			if full.Entries[1].Bytes <= full.Entries[2].Bytes {
				t.Fatal("fixture must have an oversized middle entry")
			}
			result, err := Build(snapshot, state.AccessKeyView{}, version, limit)
			if err != nil || result.IncludedCount != 1 || len(result.Entries) != 3 ||
				!result.Entries[0].Included || result.Entries[1].Included || result.Entries[2].Included ||
				result.SelectedBytes != full.SelectedBytes || result.ResponseBytes > limit {
				t.Fatalf("catalog skipped an oversized entry: %+v, err=%v", result.Entries, err)
			}
			var wire struct {
				Models []struct {
					Slug string `json:"slug"`
				} `json:"models"`
			}
			if err := json.Unmarshal(result.Body, &wire); err != nil || len(wire.Models) != 1 || wire.Models[0].Slug != "gpt-6-prefix-a" {
				t.Fatalf("wire is not the complete first-entry prefix: %v", err)
			}
		})
	}
}

func TestCatalogBudgetCountsActualEscapedJSONAndKeepsMaximalPrefix(t *testing.T) {
	for _, version := range []string{"0.159.2", "0.154.0"} {
		t.Run(version, func(t *testing.T) {
			snapshot := &state.ConfigSnapshot{
				ExecutionCandidates:  state.ExecutionCandidateIndex{protocol.OpenAIResponses: {execution.OperationResponsesCreate: {}}},
				ClientModelOverrides: map[string]catalog.ClientModelOverrides{},
			}
			for index := range 12 {
				name := fmt.Sprintf("gpt-6-test-%02d", index)
				snapshot.ExecutionCandidates[protocol.OpenAIResponses][execution.OperationResponsesCreate][name] = []state.RouteTarget{{GroupID: 1}}
				display := "中文 <model> & \"quoted\"\n\u2028"
				snapshot.ClientModelOverrides[name] = catalog.ClientModelOverrides{DisplayName: &display}
			}
			full, err := Build(snapshot, state.AccessKeyView{}, version, LimitBytes)
			if err != nil {
				t.Fatal(err)
			}
			if full.IncludedCount != 12 || full.SelectedBytes != int64(len(full.Body)) || full.ResponseBytes != full.SelectedBytes {
				t.Fatalf("full budget = %+v", full)
			}
			var wire struct {
				Models []json.RawMessage `json:"models"`
			}
			if err := json.Unmarshal(full.Body, &wire); err != nil {
				t.Fatal(err)
			}
			for index, item := range wire.Models {
				if full.Entries[index].Bytes != int64(len(item)) {
					t.Fatalf("entry %d has incorrect UTF-8/escaping size", index)
				}
			}
			for _, adjustment := range []int64{-1, 0, 1} {
				limit := full.ResponseBytes + adjustment
				result, err := Build(snapshot, state.AccessKeyView{}, version, limit)
				want := 12
				if adjustment == -1 {
					want = 11
				}
				if err != nil || result.IncludedCount != want || int64(len(result.Body)) > limit || !json.Valid(result.Body) {
					t.Fatalf("limit %d: count=%d, err=%v", limit, result.IncludedCount, err)
				}
				if result.SelectedBytes != full.SelectedBytes {
					t.Fatal("budget lost overflowing configured entries")
				}
			}
			filtered, err := Build(snapshot, state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{2: {}}}}, version, LimitBytes)
			if err != nil || string(filtered.Body) != `{"models":[]}` {
				t.Fatalf("permission-filtered catalog = %s, %v", filtered.Body, err)
			}
		})
	}
}
