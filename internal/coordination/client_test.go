package coordination

import (
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKeyUsesGatewayPrefix(t *testing.T) {
	if got := Key("config", "version"); got != "gw:config:version" {
		t.Fatalf("Key() = %q, want gw:config:version", got)
	}
}

func TestOpenRejectsInvalidDSNWithoutLeakingSecret(t *testing.T) {
	client, err := Open(context.Background(), "redis://:s3cret@[bad")
	if err == nil {
		_ = client.Close()
		t.Fatal("Open() error = nil, want invalid DSN rejection")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Open() error leaks the password: %v", err)
	}
}

func TestOpenRejectsUnknownQueryParameter(t *testing.T) {
	client, err := Open(context.Background(), "redis://127.0.0.1:6379/0?bogus=1")
	if err == nil {
		_ = client.Close()
		t.Fatal("Open() error = nil, want unknown parameter rejection")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Open() error = %v, want it to name the unknown parameter", err)
	}
}

func TestOpenFailsWhenRedisUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Open(ctx, "redis://127.0.0.1:1/0?dial_timeout=1&max_retries=-1")
	if err == nil {
		_ = client.Close()
		t.Fatal("Open() error = nil, want connection failure")
	}
	if !strings.Contains(err.Error(), "connect redis") {
		t.Fatalf("Open() error = %v, want a connect failure", err)
	}
}

func TestParseDSNSelectsTopologyFromMasterName(t *testing.T) {
	sentinel, err := parseDSN("redis://s1:26379/0?master_name=m&addr=s2:26379")
	if err != nil {
		t.Fatalf("parseDSN(sentinel) error = %v", err)
	}
	if sentinel.mode != modeSentinel || sentinel.masterName != "m" {
		t.Fatalf("parseDSN(sentinel) = %q/%q, want sentinel/m", sentinel.mode, sentinel.masterName)
	}
	if sentinel.failover == nil || sentinel.standalone != nil {
		t.Fatalf("parseDSN(sentinel) built the wrong client kind: %#v", sentinel)
	}
	wantSentinels := []string{"s1:26379", "s2:26379"}
	if !slices.Equal(sentinel.failover.SentinelAddrs, wantSentinels) {
		t.Fatalf("SentinelAddrs = %v, want %v", sentinel.failover.SentinelAddrs, wantSentinels)
	}

	standalone, err := parseDSN("redis://127.0.0.1:6379/3")
	if err != nil {
		t.Fatalf("parseDSN(standalone) error = %v", err)
	}
	if standalone.mode != modeStandalone || standalone.masterName != "" {
		t.Fatalf("parseDSN(standalone) = %q/%q, want standalone and no master", standalone.mode, standalone.masterName)
	}
	if standalone.standalone == nil || standalone.failover != nil {
		t.Fatalf("parseDSN(standalone) built the wrong client kind: %#v", standalone)
	}
	if standalone.standalone.Addr != "127.0.0.1:6379" || standalone.standalone.DB != 3 {
		t.Fatalf("standalone options = %q/%d, want 127.0.0.1:6379 db 3",
			standalone.standalone.Addr, standalone.standalone.DB)
	}
}

func TestInstanceIdentityIsRandomHex(t *testing.T) {
	first, err := newIdentity(rand.Reader)
	if err != nil {
		t.Fatalf("newIdentity() error = %v", err)
	}
	if len(first) != 32 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("holder = %q, want 32 lowercase hex characters", first)
	}
	second, err := newIdentity(rand.Reader)
	if err != nil {
		t.Fatalf("newIdentity() error = %v", err)
	}
	if first == second {
		t.Fatal("two lease holders share an identity; instances would be indistinguishable")
	}
}

func TestInstanceIdentityFailsWithoutRandomness(t *testing.T) {
	if _, err := newIdentity(strings.NewReader("too short")); err == nil {
		t.Fatal("newIdentity() error = nil, want a failure for an exhausted random source")
	}
}

// A sentinel set is normally written as a comma-separated host list, which is
// what every Sentinel client and every operator's runbook uses. go-redis reads
// only one host out of a failover URL, so without splitting it here the whole
// list is dialled as a single hostname and the instance never starts — which is
// exactly what happened the first time the Compose topology was brought up.
func TestParseDSNSplitsCommaSeparatedSentinels(t *testing.T) {
	sentinel, err := parseDSN(
		"redis://sentinel-a:26379,sentinel-b:26379,sentinel-c:26379?master_name=gptload")
	if err != nil {
		t.Fatalf("parseDSN() error = %v", err)
	}
	want := []string{"sentinel-a:26379", "sentinel-b:26379", "sentinel-c:26379"}
	if !slices.Equal(sentinel.failover.SentinelAddrs, want) {
		t.Fatalf("SentinelAddrs = %v, want %v", sentinel.failover.SentinelAddrs, want)
	}
	if sentinel.masterName != "gptload" {
		t.Fatalf("masterName = %q, want gptload", sentinel.masterName)
	}
}

// A sentinel address without a port means Sentinel's port, not the data node's.
// Defaulting to 6379 would connect to the master and then fail every sentinel
// command, which reads as a broken cluster rather than a misconfigured DSN.
func TestParseDSNDefaultsSentinelPorts(t *testing.T) {
	sentinel, err := parseDSN("redis://sentinel-a,sentinel-b:26380?master_name=gptload")
	if err != nil {
		t.Fatalf("parseDSN() error = %v", err)
	}
	want := []string{"sentinel-a:26379", "sentinel-b:26380"}
	if !slices.Equal(sentinel.failover.SentinelAddrs, want) {
		t.Fatalf("SentinelAddrs = %v, want %v", sentinel.failover.SentinelAddrs, want)
	}
}

// Both spellings of "more sentinels" have to survive together, or an operator
// who mixes them silently loses half the set.
func TestParseDSNKeepsCommaHostsAndAddrParameters(t *testing.T) {
	sentinel, err := parseDSN(
		"redis://s1:26379,s2:26379/0?master_name=m&addr=s3:26379")
	if err != nil {
		t.Fatalf("parseDSN() error = %v", err)
	}
	want := []string{"s1:26379", "s2:26379", "s3:26379"}
	if !slices.Equal(sentinel.failover.SentinelAddrs, want) {
		t.Fatalf("SentinelAddrs = %v, want %v", sentinel.failover.SentinelAddrs, want)
	}
	if sentinel.failover.DB != 0 {
		t.Fatalf("DB = %d, want 0", sentinel.failover.DB)
	}
}
