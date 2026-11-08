package cluster

import (
	"testing"
	"time"

	"gpt-load/internal/state"
)

func TestQuotaCooldownRecoveryAcrossReplicas(t *testing.T) {
	server, client := newTestClient(t)
	a := newHealthRegistry(t, healthTestEntry(1, 7))
	b := newHealthRegistry(t, healthTestEntry(1, 7))
	first := NewCredentialHealth(client, a)
	second := NewCredentialHealth(newClientForServer(t, server, "quota-peer"), b)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	ref := mustRef(t, a, 1)
	must := func(result state.SharedHealthResult, err error) state.SharedHealthResult {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	must(first.CooldownCredential(ctx, ref, now.Add(time.Minute), 0))
	must(first.CooldownModel(ctx, ref, "model", now.Add(time.Minute), now))
	must(first.RecordFailure(ctx, ref, 1))
	for _, model := range []string{"", "model"} {
		if !must(first.CooldownQuota(ctx, ref, model, now.Add(time.Hour), now)).Accepted {
			t.Fatal("quota rejected")
		}
	}
	if err := second.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	peerRef := mustRef(t, b, 1)
	if !sameView(viewOf(t, a, 1), viewOf(t, b, 1)) {
		t.Fatal("peer did not hydrate quota")
	}
	// Even a rejection with the same deadline is new evidence on another replica.
	if result := must(first.CooldownQuota(ctx, ref, "model", now.Add(time.Hour), now.Add(time.Second))); !result.Accepted || result.Changed {
		t.Fatalf("same deadline rejection = %#v", result)
	}
	for _, sample := range []time.Time{now, now.Add(time.Second)} {
		if must(second.RestoreObservedQuota(ctx, peerRef, sample.UnixMilli())).Accepted {
			t.Fatal("old/equal sample cleared newer rejection")
		}
	}
	if !must(second.RestoreObservedQuota(ctx, peerRef, now.Add(2*time.Second).UnixMilli())).Accepted {
		t.Fatal("fresh recovery rejected")
	}
	if err := first.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, registry := range []*state.CredentialRegistry{a, b} {
		view := viewOf(t, registry, 1)
		if view.cooldown != now.Add(time.Minute).UnixMilli() || view.modelCooldowns["model"] != now.Add(time.Minute).UnixMilli() || !view.blacklisted || view.failures != 1 {
			t.Fatalf("quota recovery changed ordinary health: %#v", view)
		}
	}
	if must(first.CooldownQuota(ctx, ref, "", now.Add(time.Hour), now.Add(3*time.Second))).Accepted {
		t.Fatal("old in-flight quota failure accepted")
	}
	fresh := mustRef(t, a, 1)
	must(first.CooldownQuota(ctx, fresh, "", now.Add(time.Hour), now.Add(3*time.Second)))
	if must(second.RestoreObservedQuota(ctx, peerRef, now.Add(4*time.Second).UnixMilli())).Accepted {
		t.Fatal("replayed recovery cleared new quota cooldown")
	}
	must(first.Restore(ctx, fresh, true, true))
	if err := second.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	view := viewOf(t, b, 1)
	if view.cooldown != 0 || len(view.modelCooldowns) != 0 || view.blacklisted || view.failures != 0 {
		t.Fatal("explicit restore left health state")
	}
}
