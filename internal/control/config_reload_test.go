package control

import (
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// mustReloadCommittedConfiguration applies committed database truth the way a
// remote configuration broadcast does.
func mustReloadCommittedConfiguration(t *testing.T, fixture serviceFixture) {
	t.Helper()
	if err := fixture.service.ReloadCommittedConfiguration(t.Context()); err != nil {
		t.Fatalf("ReloadCommittedConfiguration() error = %v", err)
	}
}

// commitRemoteChange writes straight to the database, standing in for a
// control transaction another instance already committed.
func commitRemoteChange(t *testing.T, db *gorm.DB, value any) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatalf("commit remote change: %v", err)
	}
}

func assertSnapshotMatchesDatabase(t *testing.T, fixture serviceFixture) {
	t.Helper()
	got := fixture.manager.Current()
	want, err := state.Compile(mustBuildCompileInput(t, fixture.db))
	if err != nil {
		t.Fatal(err)
	}
	want.Revision = got.Revision
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded snapshot differs from database\ngot=%#v\nwant=%#v", got, want)
	}
}

func TestReloadCommittedConfigurationAppliesRemoteCommit(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	beforeRevision := fixture.manager.Current().Revision

	commitRemoteChange(t, fixture.db, validControlGroup("remote-commit"))
	price := int64(3_000_000)
	commitRemoteChange(t, fixture.db, &models.ModelPrice{
		ChannelID: "openai_compatible", ModelID: "gpt-4o",
		InputPriceNanoUSDPerMillionTokens:  &price,
		OutputPriceNanoUSDPerMillionTokens: &price,
	})

	mustReloadCommittedConfiguration(t, fixture)

	after := fixture.manager.Current()
	if after.Revision != beforeRevision+1 {
		t.Fatalf("snapshot revision = %d, want %d", after.Revision, beforeRevision+1)
	}
	if len(after.Groups) != 1 {
		t.Fatalf("reloaded groups = %#v, want the remotely committed group", after.Groups)
	}
	assertSnapshotMatchesDatabase(t, fixture)

	table := fixture.priceRuntime.Load()
	if table == nil {
		t.Fatal("priceRuntime.Load() = nil after reload")
	}
	identity, err := PriceIdentityForChannelModel("openai_compatible", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Lookup(pricing.Identity(identity)); !ok {
		t.Fatal("reloaded price table is missing the remotely committed model price")
	}
}

func TestReloadCommittedConfigurationWithoutChangesPublishesNothing(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	commitRemoteChange(t, fixture.db, validControlGroup("quiet-reload"))
	mustReloadCommittedConfiguration(t, fixture)

	before, updates := fixture.manager.CurrentWithUpdates()
	mustReloadCommittedConfiguration(t, fixture)

	after := fixture.manager.Current()
	if after != before {
		t.Fatalf("unchanged reload replaced the snapshot: revision %d -> %d", before.Revision, after.Revision)
	}
	select {
	case <-updates:
		t.Fatal("unchanged reload closed the updates channel")
	default:
	}
}

func TestReloadCommittedConfigurationPreservesCredentialRuntimeHealth(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	group := validControlGroup("health-preserving")
	commitRemoteChange(t, fixture.db, group)
	commitRemoteChange(t, fixture.db, &models.Credential{
		GroupID: group.ID, Data: "ciphertext", Fingerprint: "credential-fingerprint",
		Status: models.CredentialStatusActive,
	})
	mustReloadCommittedConfiguration(t, fixture)

	cooldown := time.Now().Add(time.Hour).UTC()
	if !fixture.registry.SetCooldown(1, cooldown) {
		t.Fatal("SetCooldown(1) = false; the reload did not load the credential")
	}
	if _, ok := fixture.registry.IncrFailure(1); !ok {
		t.Fatal("IncrFailure(1) = false")
	}

	// An unrelated remote setting change must not cost this instance the
	// credential health only it knows about.
	commitRemoteChange(t, fixture.db, &models.SystemSetting{
		Key: state.SettingBlacklistThreshold, Value: "9",
	})
	beforeRevision := fixture.manager.Current().Revision
	mustReloadCommittedConfiguration(t, fixture)

	if revision := fixture.manager.Current().Revision; revision != beforeRevision+1 {
		t.Fatalf("snapshot revision = %d, want %d", revision, beforeRevision+1)
	}
	assertSnapshotMatchesDatabase(t, fixture)
	got, ok := fixture.registry.CredentialCooldownUntil(1)
	if !ok || !got.Equal(cooldown) {
		t.Fatalf("CredentialCooldownUntil(1) = %v, %t, want %v, true", got, ok, cooldown)
	}
	if snapshots := fixture.registry.Snapshot(); len(snapshots) != 1 || snapshots[0].FailureCount != 1 {
		t.Fatalf("reload reset credential failure count: %#v", snapshots)
	}
}

func TestReloadCommittedConfigurationPreservesAccessQuotaUsage(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	prefix := "sk-gl-"
	accessKey := &models.AccessKey{
		Name: "quota-holder", KeyValue: "ciphertext", KeyHash: "quota-holder-hash",
		KeyPrefix: &prefix, KeySuffix: "abcd",
		Status: string(state.AccessKeyStatusActive), Filters: models.JSON(`{}`),
	}
	commitRemoteChange(t, fixture.db, accessKey)
	rule := &models.AccessKeyCostLimitRule{
		AccessKeyID: accessKey.ID, Kind: models.AccessKeyCostLimitKindTotal,
		LimitNanoUSD: 100_000_000_000, RuleRevision: 1,
	}
	commitRemoteChange(t, fixture.db, rule)
	mustReloadCommittedConfiguration(t, fixture)

	now := time.Now()
	ticket, decision := fixture.accessQuota.Admit(accessKey.ID, now)
	if !decision.Allowed || len(ticket.Rules) != 1 {
		t.Fatalf("Admit() = %#v, %#v; the reload did not install the cost limit rule", ticket, decision)
	}
	const consumed = int64(7_000_000_000)
	fixture.accessQuota.Complete(ticket, consumed)
	if used := quotaUsedNanoUSD(t, fixture.accessQuota, accessKey.ID, now, rule.ID); used != consumed {
		t.Fatalf("used = %d before reload, want %d", used, consumed)
	}

	commitRemoteChange(t, fixture.db, validControlGroup("unrelated-remote-change"))
	mustReloadCommittedConfiguration(t, fixture)

	if used := quotaUsedNanoUSD(t, fixture.accessQuota, accessKey.ID, now, rule.ID); used != consumed {
		t.Fatalf("used = %d after reload, want the preserved %d", used, consumed)
	}
}

func quotaUsedNanoUSD(
	t *testing.T,
	runtime *accessquota.Runtime,
	accessKeyID uint,
	now time.Time,
	ruleID uint,
) int64 {
	t.Helper()
	for _, rule := range runtime.Snapshot(accessKeyID, now).Rules {
		if rule.ID == ruleID {
			return rule.UsedNanoUSD
		}
	}
	t.Fatalf("access quota rule %d is absent from the runtime view", ruleID)
	return 0
}
