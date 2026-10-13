package control

import (
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/testutil/clustertest"
)

func TestAccessKeyCostLimitRuntimeProjectionIsSharedByCollectionHomeAndHealth(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name: "limited",
		CostLimitRules: OptionalAccessKeyCostLimitRules{Set: true, Values: []AccessKeyCostLimitRuleRequest{
			{Kind: accessquota.KindTotal, LimitUSD: "100"},
			{Kind: accessquota.KindPeriodic, LimitUSD: "20", PeriodSeconds: 300},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_787_184_000, 0).UTC()
	fixture.service.now = func() time.Time { return now }
	admitAccessQuota(t, fixture, created.ID, now, 100_000_000_000)

	collection, err := fixture.service.ListAccessKeyCollection(
		t.Context(),
		AccessKeyCollectionQuery{Page: 1, PageSize: 20},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Items) != 1 || collection.Items[0].CostLimitStatus == nil ||
		collection.Items[0].CostLimitStatus.Allowed ||
		len(collection.Items[0].CostLimitStatus.Rules) != 2 {
		t.Fatalf("collection cost limit projection = %#v", collection.Items)
	}

	health, err := fixture.service.RuntimeHealth()
	if err != nil {
		t.Fatal(err)
	}
	if len(health.BlockedAccessKeys) != 1 || health.BlockedAccessKeys[0].AccessKeyID != created.ID ||
		health.BlockedAccessKeys[0].Recoverable || len(health.BlockedAccessKeys[0].BlockingRules) != 2 {
		t.Fatalf("blocked access keys = %#v", health.BlockedAccessKeys)
	}
	if health.Counts != (healthCountsResponse{}) {
		t.Fatalf("business quota block changed credential health counts = %#v", health.Counts)
	}
}

func TestAccessKeyCostLimitProjectionReadsSharedClusterState(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	peer := clustertest.Connect(t, fixture.redis, "node-peer")
	shared := cluster.NewAccessQuota(peer, requestlog.AccessQuotaStateReader{DB: fixture.db})

	created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name: "limited",
		CostLimitRules: OptionalAccessKeyCostLimitRules{Set: true, Values: []AccessKeyCostLimitRuleRequest{
			{Kind: accessquota.KindTotal, LimitUSD: "100"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_787_184_000, 0).UTC()
	fixture.service.now = func() time.Time { return now }
	// Another instance spent the whole limit through its own quota store.
	ticket, decision, err := shared.Admit(t.Context(), fixture.manager.Current(), created.ID, now)
	if err != nil || !decision.Allowed {
		t.Fatalf("Admit() = %#v, %v", decision, err)
	}
	if _, err := shared.Complete(t.Context(), ticket, 100_000_000_000); err != nil {
		t.Fatal(err)
	}

	collection, err := fixture.service.ListAccessKeyCollection(t.Context(), AccessKeyCollectionQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Items) != 1 || collection.Items[0].CostLimitStatus == nil ||
		collection.Items[0].CostLimitStatus.Allowed {
		t.Fatalf("collection cost limit projection = %#v", collection.Items)
	}
	health, err := fixture.service.RuntimeHealth()
	if err != nil {
		t.Fatal(err)
	}
	if len(health.BlockedAccessKeys) != 1 || health.BlockedAccessKeys[0].AccessKeyID != created.ID {
		t.Fatalf("blocked access keys = %#v", health.BlockedAccessKeys)
	}

	fixture.redis.Close()
	if _, err := fixture.service.ListAccessKeyCollection(t.Context(), AccessKeyCollectionQuery{Page: 1, PageSize: 20}); err == nil {
		t.Fatal("ListAccessKeyCollection() with Redis down error = nil")
	}
	if _, err := fixture.service.RuntimeHealth(); err == nil {
		t.Fatal("RuntimeHealth() with Redis down error = nil")
	}
}
