package control

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpt-load/internal/state"
	"gpt-load/internal/testutil/pgtest"
)

func TestCredentialNamePropagatesAcrossReplicasWithoutChangingRuntime(t *testing.T) {
	_, redisAddr := externalClusterTarget(t)
	dsn := pgtest.NewDatabase(t)
	prefix := fmt.Sprintf("name-test-%d", time.Now().UnixNano())
	a := newClusterContractInstance(t, dsn, redisAddr, prefix, "name-a")
	b := newClusterContractInstance(t, dsn, redisAddr, prefix, "name-b")
	groupID := createGroupWithCredentials(t, a.fixture, "sk-name-cluster")
	if _, err := b.fixture.service.reloadCommittedConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := registryViewsForGroup(b.fixture.registry, groupID)[0].ID
	if !b.fixture.registry.SetBlacklisted(id) {
		t.Fatal("blacklist")
	}
	before := b.fixture.registry.Snapshot()
	configRevision := b.fixture.manager.Current().Revision
	syncB := NewClusterConfigSync(b.fixture.service, b.bus)
	syncB.pollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); syncB.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("sync did not stop")
		}
	})
	for index, name := range []string{"cluster alias", ""} {
		if _, err := a.fixture.service.UpdateGroupCredential(t.Context(), groupID, id, CredentialUpdateRequest{
			Name: optionalField[string]{Set: true, Value: name},
		}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			ref, ok := b.fixture.registry.CredentialRef(id)
			if ok && ref.Name == name {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("peer alias = %q, want %q", ref.Name, name)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if revision, err := readClusterConfigRevision(t.Context(), a.fixture.db); err != nil || revision != uint64(index+2) {
			t.Fatalf("revision = %d, %v", revision, err)
		}
		if !reflect.DeepEqual(before, b.fixture.registry.Snapshot()) {
			t.Fatal("rename changed peer runtime")
		}
		if b.fixture.manager.Current().Revision != configRevision {
			t.Fatal("rename changed routing snapshot")
		}
		// A fresh registry reads the same persisted display metadata.
		fresh := newServiceFixtureWithDSN(t, dsn)
		if _, err := fresh.service.reloadCommittedConfig(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ref, ok := fresh.registry.CredentialRef(id); !ok || ref.Name != name {
			t.Fatalf("restart alias = %#v", ref)
		}
	}
}

func TestCredentialNameMixedUpdatePreservesConcurrencyAndSorting(t *testing.T) {
	pair := newClusterPair(t)
	groupID := createGroupWithCredentials(t, pair.a, "sk-name-one\nsk-name-two")
	pair.reloadB(t)
	views := registryViewsForGroup(pair.a.registry, groupID)
	for index, name := range []string{"Zulu", "Alpha"} {
		id := views[index].ID
		item, err := pair.a.service.UpdateGroupCredential(t.Context(), groupID, id, CredentialUpdateRequest{
			Name:             optionalField[string]{Set: true, Value: name},
			ConcurrencyLimit: optionalField[int]{Set: true, Value: index + 2},
		})
		if err != nil || item.Name != name {
			t.Fatalf("mixed update = %#v, %v", item, err)
		}
		pair.reloadB(t)
		for _, registry := range []*state.CredentialRegistry{pair.a.registry, pair.b.registry} {
			ref, ok := registry.CredentialRef(id)
			if !ok || ref.Name != name || ref.ConcurrencyLimit != index+2 {
				t.Fatalf("mixed runtime = %#v", ref)
			}
		}
	}
	result, err := pair.a.service.ListGroupCredentials(t.Context(), groupID, CredentialCollectionQuery{
		Page: 1, PageSize: 20, modern: &modernCredentialFilters{sort: "name"},
	})
	if err != nil || len(result.Items) != 2 || result.Items[0].Name != "Alpha" || result.Items[1].Name != "Zulu" {
		t.Fatalf("name sort = %#v, %v", result, err)
	}
	// A missing name field must leave the alias unchanged.
	if item, err := pair.a.service.UpdateGroupCredential(t.Context(), groupID, views[0].ID, CredentialUpdateRequest{
		ConcurrencyLimit: optionalField[int]{Set: true, Value: 0},
	}); err != nil || item.Name != "Zulu" {
		t.Fatalf("omitted name = %#v, %v", item, err)
	}
}
