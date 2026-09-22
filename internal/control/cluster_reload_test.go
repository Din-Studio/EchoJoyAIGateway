package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/sqlitetest"
)

// clusterPair models two instances sharing one database with independent
// in-memory runtime state. Instance A writes; instance B reloads.
type clusterPair struct {
	a serviceFixture
	b serviceFixture
}

func newClusterPair(t *testing.T) clusterPair {
	t.Helper()
	db := sqlitetest.OpenMigrated(t)
	pair := clusterPair{
		a: newServiceFixtureWithDatabase(t, db),
		b: newServiceFixtureWithDatabase(t, db),
	}
	pair.a.service.clusterEvents = &recordingConfigEventPublisher{instance: "node-a"}
	pair.b.service.clusterEvents = &recordingConfigEventPublisher{instance: "node-b"}
	return pair
}

func (pair clusterPair) reloadB(t *testing.T) uint64 {
	t.Helper()
	revision, err := pair.b.service.reloadCommittedConfig(t.Context())
	if err != nil {
		t.Fatalf("reloadCommittedConfig() error = %v", err)
	}
	return revision
}

func createCompatibleGroup(t *testing.T, fixture serviceFixture, baseURL, credential string) uint {
	t.Helper()
	name := fmt.Sprintf("compatible-group-%d", testIdempotencySequence.Add(1))
	result, err := fixture.service.CreateGroup(t.Context(), GroupCreateRequest{
		Name: &name, ChannelID: channel.OpenAICompatible, ConnectionType: models.ConnectionTypeAPIKey,
		Params:      json.RawMessage(fmt.Sprintf(`{"base_url":%q}`, baseURL)),
		Models:      optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-4o"}}},
		Credentials: credential,
	})
	if err != nil {
		t.Fatalf("CreateGroup(%q) error = %v", name, err)
	}
	return result.GroupID
}

func snapshotHasGroup(snapshot *state.ConfigSnapshot, groupID uint) bool {
	if snapshot == nil {
		return false
	}
	for _, group := range snapshot.Groups {
		if group.ID == groupID {
			return true
		}
	}
	return false
}

func registryViewsForGroup(registry *state.CredentialRegistry, groupID uint) []state.CredentialRuntimeView {
	var views []state.CredentialRuntimeView
	for _, view := range registry.Snapshot() {
		if view.GroupID == groupID {
			views = append(views, view)
		}
	}
	return views
}

func schedulingMembersForGroup(registry *state.CredentialRegistry, groupID uint) int {
	count := 0
	registry.SchedulingState().WithLock(func(ledger *state.SchedulingLedger) {
		for _, member := range ledger.Members {
			if member.GroupID == groupID {
				count++
			}
		}
	})
	return count
}

func TestClusterReloadAppliesRemoteGroupAndCredentials(t *testing.T) {
	pair := newClusterPair(t)
	groupID := createGroupWithCredentials(t, pair.a, "sk-remote")
	if snapshotHasGroup(pair.b.manager.Current(), groupID) {
		t.Fatal("instance B saw the group before reloading")
	}

	revision := pair.reloadB(t)
	if revision != 1 {
		t.Fatalf("reload revision = %d, want 1", revision)
	}
	if !snapshotHasGroup(pair.b.manager.Current(), groupID) {
		t.Fatal("instance B snapshot does not contain the remote group")
	}
	views := registryViewsForGroup(pair.b.registry, groupID)
	if len(views) != 1 {
		t.Fatalf("instance B registry views for group %d = %d, want 1", groupID, len(views))
	}
	if schedulingMembersForGroup(pair.b.registry, groupID) != 1 {
		t.Fatal("instance B scheduling ledger does not contain the remote credential")
	}
}

func TestClusterReloadPreservesRuntimeStateAndBumpsRevisionOnce(t *testing.T) {
	pair := newClusterPair(t)
	steadyGroup := createGroupWithCredentials(t, pair.a, "sk-steady")
	changedGroup := createCompatibleGroup(t, pair.a, "https://changed.example/v1", "sk-changed")
	pair.reloadB(t)

	steady := registryViewsForGroup(pair.b.registry, steadyGroup)
	changed := registryViewsForGroup(pair.b.registry, changedGroup)
	if len(steady) != 1 || len(changed) != 1 {
		t.Fatalf("registry views steady=%d changed=%d, want 1/1", len(steady), len(changed))
	}
	now := time.Now()
	cooldownUntil := now.Add(10 * time.Minute).Truncate(time.Millisecond)
	if !pair.b.registry.SetCooldown(steady[0].ID, cooldownUntil) {
		t.Fatal("SetCooldown() = false")
	}
	if _, ok := pair.b.registry.IncrFailure(steady[0].ID); !ok {
		t.Fatal("IncrFailure() = false")
	}
	ref, ok := pair.b.registry.CredentialRef(steady[0].ID)
	if !ok {
		t.Fatal("CredentialRef() = false")
	}
	modelCooldownUntil := now.Add(5 * time.Minute)
	if applied, _ := pair.b.registry.SetModelCooldown(ref, "gpt-4o", modelCooldownUntil, now); !applied {
		t.Fatal("SetModelCooldown() = false")
	}
	if !pair.b.registry.SetBlacklisted(changed[0].ID) {
		t.Fatal("SetBlacklisted() = false")
	}
	before := pair.b.manager.Current().Revision

	if _, err := pair.a.service.UpdateGroupModels(t.Context(), changedGroup, GroupModelsUpdateRequest{
		Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-4o"}, {ID: "gpt-4.1"}}},
	}); err != nil {
		t.Fatalf("UpdateGroupModels() error = %v", err)
	}
	revision := pair.reloadB(t)
	if revision != 3 {
		t.Fatalf("reload revision = %d, want 3", revision)
	}

	after := pair.b.manager.Current()
	if after.Revision != before+1 {
		t.Fatalf("snapshot revision = %d, want %d", after.Revision, before+1)
	}
	steadyAfter := registryViewsForGroup(pair.b.registry, steadyGroup)
	if len(steadyAfter) != 1 {
		t.Fatalf("steady views after reload = %d, want 1", len(steadyAfter))
	}
	if !steadyAfter[0].CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("CooldownUntil = %v, want %v", steadyAfter[0].CooldownUntil, cooldownUntil)
	}
	if steadyAfter[0].FailureCount != 1 {
		t.Fatalf("FailureCount = %d, want 1", steadyAfter[0].FailureCount)
	}
	if cooldowns := pair.b.registry.ModelCooldowns(steady[0].ID, now); !cooldowns["gpt-4o"].Equal(modelCooldownUntil) {
		t.Fatalf("ModelCooldowns = %v, want gpt-4o until %v", cooldowns, modelCooldownUntil)
	}
	changedAfter := registryViewsForGroup(pair.b.registry, changedGroup)
	if len(changedAfter) != 1 || !changedAfter[0].Blacklisted {
		t.Fatalf("changed views after reload = %#v, want one blacklisted credential", changedAfter)
	}
	var group models.Group
	if err := pair.b.db.First(&group, changedGroup).Error; err != nil {
		t.Fatalf("query changed group: %v", err)
	}
	for _, snapshotGroup := range after.Groups {
		if snapshotGroup.ID == changedGroup && len(snapshotGroup.Models) != 2 {
			t.Fatalf("instance B group models = %#v, want 2 models", snapshotGroup.Models)
		}
	}
}

func TestClusterReloadWithoutChangesLeavesRevisionUntouched(t *testing.T) {
	pair := newClusterPair(t)
	createGroupWithCredentials(t, pair.a, "sk-steady")
	first := pair.reloadB(t)
	before := pair.b.manager.Current().Revision

	second := pair.reloadB(t)
	if second != first {
		t.Fatalf("second reload revision = %d, want %d", second, first)
	}
	if got := pair.b.manager.Current().Revision; got != before {
		t.Fatalf("snapshot revision changed on no-op reload: %d -> %d", before, got)
	}
}

func TestClusterReloadRemovesDeletedGroup(t *testing.T) {
	pair := newClusterPair(t)
	groupID := createGroupWithCredentials(t, pair.a, "sk-doomed")
	pair.reloadB(t)
	if schedulingMembersForGroup(pair.b.registry, groupID) != 1 {
		t.Fatal("instance B scheduling ledger missing the credential before deletion")
	}

	if err := pair.a.service.DeleteGroup(t.Context(), groupID); err != nil {
		t.Fatalf("DeleteGroup() error = %v", err)
	}
	pair.reloadB(t)

	if snapshotHasGroup(pair.b.manager.Current(), groupID) {
		t.Fatal("instance B snapshot still contains the deleted group")
	}
	if views := registryViewsForGroup(pair.b.registry, groupID); len(views) != 0 {
		t.Fatalf("instance B registry still holds %d credentials for deleted group", len(views))
	}
	if schedulingMembersForGroup(pair.b.registry, groupID) != 0 {
		t.Fatal("instance B scheduling ledger still holds members of the deleted group")
	}
}

func TestClusterReloadAppliesRemoteModelPrices(t *testing.T) {
	pair := newClusterPair(t)
	mustEnsureInitialPrices(t, pair.b)
	createGroupWithCredentials(t, pair.a, "sk-priced")
	pair.reloadB(t)

	input := int64(1_500_000_000)
	err := pair.a.service.withControlTransaction(t.Context(), func(tx *gorm.DB) error {
		var row models.ModelPrice
		err := tx.Where("channel_id = ? AND model_id = ?", string(channel.OpenAI), "gpt-4o").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&models.ModelPrice{
				ChannelID: string(channel.OpenAI), ModelID: "gpt-4o",
				InputPriceNanoUSDPerMillionTokens: &input, IsManual: true,
			}).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&row).Updates(map[string]any{
			"input_price_nano_usd_per_million_tokens": input, "is_manual": true,
		}).Error
	})
	if err != nil {
		t.Fatalf("write model price: %v", err)
	}
	pair.reloadB(t)

	table := pair.b.priceRuntime.Load()
	if table == nil {
		t.Fatal("instance B price table is nil")
	}
	identity, err := PriceIdentityForChannelModel(string(channel.OpenAI), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := table.Lookup(identity)
	if !ok {
		t.Fatal("instance B price table missing openai/gpt-4o")
	}
	if !rule.Prices.Input.Set || rule.Prices.Input.NanoUSDPerMillion != pricing.NanoUSD(input) || !rule.IsManual {
		t.Fatalf("instance B price rule = %#v, want manual input %d", rule, input)
	}
}
