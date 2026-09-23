package control

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/requestlog"
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
	ticket, decision := fixture.accessQuota.Admit(created.ID, now)
	if !decision.Allowed {
		t.Fatalf("Admit() = %#v", decision)
	}
	fixture.accessQuota.Complete(ticket, 100_000_000_000)

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

	home, err := fixture.service.ReadAccessKeyHomeBase(t.Context(), now.UnixMilli(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if home.CurrentAccessKey == nil || home.CurrentAccessKey.CostLimitStatus == nil ||
		home.CurrentAccessKey.CostLimitStatus.Allowed ||
		home.CurrentAccessKey.Status != "active" {
		t.Fatalf("current access key projection = %#v", home.CurrentAccessKey)
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
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "control-test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	shared := cluster.NewAccessQuota(client, requestlog.AccessQuotaStateReader{DB: fixture.db})
	fixture.service.clusterQuota = shared

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
	// Another instance spent the whole limit; this instance's runtime never saw it.
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
	home, err := fixture.service.ReadAccessKeyHomeBase(t.Context(), now.UnixMilli(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if home.CurrentAccessKey == nil || home.CurrentAccessKey.CostLimitStatus == nil ||
		home.CurrentAccessKey.CostLimitStatus.Allowed || len(home.CurrentAccessKey.CostLimitRules) != 1 {
		t.Fatalf("current access key projection = %#v", home.CurrentAccessKey)
	}
	health, err := fixture.service.RuntimeHealth()
	if err != nil {
		t.Fatal(err)
	}
	if len(health.BlockedAccessKeys) != 1 || health.BlockedAccessKeys[0].AccessKeyID != created.ID {
		t.Fatalf("blocked access keys = %#v", health.BlockedAccessKeys)
	}

	server.Close()
	if _, err := fixture.service.ListAccessKeyCollection(t.Context(), AccessKeyCollectionQuery{Page: 1, PageSize: 20}); err == nil {
		t.Fatal("ListAccessKeyCollection() with Redis down error = nil")
	}
	if _, err := fixture.service.ReadAccessKeyHomeBase(t.Context(), now.UnixMilli(), created.ID); err == nil {
		t.Fatal("ReadAccessKeyHomeBase() with Redis down error = nil")
	}
	if _, err := fixture.service.RuntimeHealth(); err == nil {
		t.Fatal("RuntimeHealth() with Redis down error = nil")
	}
}
