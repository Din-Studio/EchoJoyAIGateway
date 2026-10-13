package loader_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"gpt-load/internal/protocol"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
	"gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

func TestLoaderNarrowsTeamKeysToGrantedGroups(t *testing.T) {
	db := openMigratedDatabase(t)
	g1 := createRuntimeGroup(t, db, "g1", protocol.OpenAICompletions, "m1")
	g2 := createRuntimeGroup(t, db, "g2", protocol.OpenAICompletions, "m2")
	createRuntimeGroup(t, db, "g3", protocol.OpenAICompletions, "m3")
	for _, id := range []uint{g1.ID, g2.ID} {
		mustCreate(t, db, &models.TeamGroupGrant{TeamID: "team-a", GroupID: id})
	}
	newKey := func(name string, team *string, groups string) models.AccessKey {
		key := models.AccessKey{
			Name: name, KeyValue: "cipher", KeyHash: name + "-hash", KeySuffix: "0000",
			Status: "active", Filters: models.JSON(`{"groups":` + groups + `}`), TeamID: team,
		}
		if team != nil {
			key.OwnerUserID = stringPtr("user-1")
		}
		mustCreate(t, db, &key)
		return key
	}
	platform := newKey("platform", nil, "[]")
	teamAll := newKey("team-all", stringPtr("team-a"), "[]")
	teamNarrow := newKey("team-narrow", stringPtr("team-a"), fmt.Sprintf("[%d,%d]", g2.ID, g2.ID+1))
	teamOutside := newKey("team-outside", stringPtr("team-a"), fmt.Sprintf("[%d]", g2.ID+1))
	ungranted := newKey("ungranted", stringPtr("team-b"), "[]")

	manager := state.NewManager()
	if err := loader.New(db, manager, state.NewCredentialRegistry()).Load(t.Context()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	snapshot := manager.Current()
	groupsOf := func(key models.AccessKey) []uint {
		var ids []uint
		for id := range snapshot.AccessKeysByID[key.ID].Filters.Groups {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids
	}
	tests := []struct {
		key        models.AccessKey
		wantStatus state.AccessKeyStatus
		wantGroups []uint
	}{
		{key: platform, wantStatus: state.AccessKeyStatusActive},
		{key: teamAll, wantStatus: state.AccessKeyStatusActive, wantGroups: []uint{g1.ID, g2.ID}},
		{key: teamNarrow, wantStatus: state.AccessKeyStatusActive, wantGroups: []uint{g2.ID}},
		{key: teamOutside, wantStatus: state.AccessKeyStatusDisabled},
		{key: ungranted, wantStatus: state.AccessKeyStatusDisabled},
	}
	for _, test := range tests {
		got := snapshot.AccessKeysByID[test.key.ID]
		if got.Status != test.wantStatus || !reflect.DeepEqual(groupsOf(test.key), test.wantGroups) {
			t.Errorf("%s: status=%s groups=%v, want %s %v", test.key.Name, got.Status, groupsOf(test.key), test.wantStatus, test.wantGroups)
		}
		_, routable := snapshot.AccessKeysByHash[test.key.KeyHash]
		if routable != (test.wantStatus == state.AccessKeyStatusActive) {
			t.Errorf("%s: routable = %v", test.key.Name, routable)
		}
	}

	inspection, err := scheduler.Inspect(snapshot, nil, scheduler.Query{
		AccessKey:      snapshot.AccessKeysByID[ungranted.ID],
		ClientProtocol: protocol.OpenAICompletions,
		ExternalModel:  stringPtr("m1"),
	}, time.Now())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspection.Reason != scheduler.ReasonAccessKeyDisabled {
		t.Fatalf("ungranted key reason = %s, want %s", inspection.Reason, scheduler.ReasonAccessKeyDisabled)
	}
}
