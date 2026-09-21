package coordination

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
)

// TestExternalRedisQuotaLedgerSharesOneBudget is opt-in so ordinary unit tests
// stay hermetic. It proves the property the type exists for: two runtimes
// spend one budget, not one each.
func TestExternalRedisQuotaLedgerSharesOneBudget(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalQuotaAccessKey(t, client, 1)
	ledger := NewQuotaLedger(client)

	rule := accessquota.Rule{ID: 1, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000}
	first := externalQuotaRuntime(t, ledger, accessKeyID, rule)
	second := externalQuotaRuntime(t, ledger, accessKeyID, rule)

	// One instance spends the whole budget; the other never saw the requests.
	for range 10 {
		ticket, decision := first.Admit(accessKeyID, time.Now())
		if !decision.Allowed {
			t.Fatalf("Admit() on the spending instance = %#v, want allowed", decision)
		}
		first.Complete(ticket, 100)
	}
	if _, decision := second.Admit(accessKeyID, time.Now()); decision.Allowed {
		t.Fatal("the peer instance admitted against an exhausted shared budget")
	}
}

// Concurrency is where a read-then-write ledger breaks. Total spending must
// not exceed the limit by more than the requests already in flight when it
// ran out, which with a serialized charge means not at all.
func TestExternalRedisQuotaLedgerChargesExactlyOnce(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalQuotaAccessKey(t, client, 2)
	ledger := NewQuotaLedger(client)

	rule := accessquota.Rule{ID: 2, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000_000}
	const instances = 4
	runtimes := make([]*accessquota.Runtime, 0, instances)
	for range instances {
		runtimes = append(runtimes, externalQuotaRuntime(t, ledger, accessKeyID, rule))
	}

	var charged atomic.Int64
	done := make(chan struct{})
	for _, runtime := range runtimes {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 25 {
				ticket, decision := runtime.Admit(accessKeyID, time.Now())
				if !decision.Allowed {
					continue
				}
				runtime.Complete(ticket, 7)
				charged.Add(7)
			}
		}()
	}
	for range runtimes {
		<-done
	}

	view := runtimes[0].Snapshot(accessKeyID, time.Now())
	if view.Rules[0].UsedNanoUSD != charged.Load() {
		t.Fatalf("shared counter = %d, want exactly the %d charged across instances",
			view.Rules[0].UsedNanoUSD, charged.Load())
	}
}

// A periodic window is rolled by whichever instance ticks first, and every
// other instance has to land in that same window rather than open its own.
func TestExternalRedisQuotaLedgerRollsOneWindowForEveryInstance(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalQuotaAccessKey(t, client, 3)
	ledger := NewQuotaLedger(client)

	rule := accessquota.Rule{
		ID: 3, Revision: 1, Kind: accessquota.KindPeriodic,
		LimitNanoUSD: 1_000, PeriodSeconds: accessquota.MinPeriodSeconds,
	}
	first := externalQuotaRuntime(t, ledger, accessKeyID, rule)
	second := externalQuotaRuntime(t, ledger, accessKeyID, rule)

	now := time.Now()
	firstTicket, decision := first.Admit(accessKeyID, now)
	if !decision.Allowed {
		t.Fatalf("first Admit() = %#v, want allowed", decision)
	}
	secondTicket, decision := second.Admit(accessKeyID, now)
	if !decision.Allowed {
		t.Fatalf("peer Admit() = %#v, want allowed", decision)
	}
	if firstTicket.Rules[0].WindowGeneration != secondTicket.Rules[0].WindowGeneration {
		t.Fatalf("window generations %d and %d differ; the instances opened separate windows",
			firstTicket.Rules[0].WindowGeneration, secondTicket.Rules[0].WindowGeneration)
	}

	first.Complete(firstTicket, 600)
	second.Complete(secondTicket, 500)
	if _, decision := first.Admit(accessKeyID, now); decision.Allowed {
		t.Fatal("the window admitted past its limit after both instances charged it")
	}
}

// Admission is the authoritative gate, so an unreachable ledger has to refuse
// with an outage the caller can distinguish from an exhausted budget.
func TestExternalRedisQuotaLedgerFailsClosedWhenRedisIsGone(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalQuotaAccessKey(t, client, 4)
	rule := accessquota.Rule{ID: 4, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000}
	if _, decision := externalQuotaRuntime(t, NewQuotaLedger(client), accessKeyID, rule).
		Admit(accessKeyID, time.Now()); !decision.Allowed {
		t.Fatalf("Admit() against a live Redis = %#v, want allowed", decision)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	severed, err := Open(ctx, redisTestDSN(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := severed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	runtime := externalQuotaRuntime(t, NewQuotaLedger(severed), accessKeyID, rule)
	_, decision := runtime.Admit(accessKeyID, time.Now())
	if decision.Allowed {
		t.Fatal("Admit() spent a budget it could not read")
	}
	if !decision.Unavailable {
		t.Fatal("Unavailable = false; the outage would be reported as an exhausted budget")
	}
}

func externalQuotaRuntime(
	t *testing.T,
	ledger *QuotaLedger,
	accessKeyID uint,
	rule accessquota.Rule,
) *accessquota.Runtime {
	t.Helper()
	runtime := accessquota.NewRuntime()
	if err := runtime.Reconcile(map[uint][]accessquota.Rule{accessKeyID: {rule}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	runtime.SetSharedLedger(ledger)
	return runtime
}

// externalQuotaAccessKey namespaces the counter so parallel CI jobs cannot
// spend each other's budgets.
func externalQuotaAccessKey(t *testing.T, client *Client, ruleID uint) uint {
	t.Helper()
	accessKeyID := uint(time.Now().UnixNano() % 1_000_000_007)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().Del(ctx, accessQuotaKey(accessKeyID, ruleID)).Err(); err != nil {
			t.Logf("delete test quota key: %v", err)
		}
	})
	return accessKeyID
}
