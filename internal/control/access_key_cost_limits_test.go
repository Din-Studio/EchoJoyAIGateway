package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage/models"
)

func TestAccessKeyCostLimitRuleSubresourceCreatesUpdatesAndDeletesOneRule(t *testing.T) {
	t.Parallel()
	fixture, engine := newAccessKeyCostLimitHTTPFixture(t)
	created := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost, "/api/access-keys", `{
		"name":"limited",
		"cost_limit_rules":[
			{"kind":"total","limit_usd":"100"},
			{"kind":"periodic","limit_usd":"20","period_seconds":18000},
			{"kind":"periodic","limit_usd":"30","period_seconds":86400}
		]
	}`, "00000000-0000-4000-8000-000000009001")
	if created.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", created.Code, created.Body.String())
	}
	var createdEnvelope struct {
		Data AccessKeyCreateResult `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdEnvelope); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	accessKeyID := createdEnvelope.Data.ID
	rules := loadAccessKeyCostLimitRules(t, fixture, accessKeyID)
	if len(rules) != 3 {
		t.Fatalf("stored rules = %#v", rules)
	}
	total, fiveHours, day := rules[0], rules[1], rules[2]
	if err := fixture.db.Model(&models.AccessKeyCostLimitState{}).
		Where("rule_id = ?", fiveHours.ID).
		Updates(map[string]any{
			"used_nano_usd":        int64(15_000_000_000),
			"window_started_at_ms": int64(1_787_184_000_000),
			"window_ends_at_ms":    int64(1_787_202_000_000),
			"window_generation":    uint64(2),
			"snapshot_version":     uint64(4),
		}).Error; err != nil {
		t.Fatalf("seed periodic state: %v", err)
	}
	rulePath := func(ruleID uint) string {
		return fmt.Sprintf("/api/access-keys/%d/cost-limits/%d", accessKeyID, ruleID)
	}
	decodeRules := func(response *httptest.ResponseRecorder) []AccessKeyCostLimitRule {
		t.Helper()
		var envelope struct {
			Data AccessKeyMetadata `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode rule response: %v", err)
		}
		if envelope.Data.ID != accessKeyID {
			t.Fatalf("rule response access key = %d, want %d", envelope.Data.ID, accessKeyID)
		}
		return envelope.Data.CostLimitRules
	}

	// Changing only the limit keeps the rule identity and its usage.
	amount := serveAccessKeyCostLimitRequest(t, engine, http.MethodPut, rulePath(fiveHours.ID),
		`{"kind":"periodic","limit_usd":"25","period_seconds":18000}`, "")
	if amount.Code != http.StatusOK {
		t.Fatalf("PUT rule amount = %d %s, want 200", amount.Code, amount.Body.String())
	}
	if got := decodeRules(amount); len(got) != 3 || got[1].ID != fiveHours.ID || got[1].LimitUSD != "25" {
		t.Fatalf("rules after amount change = %#v", got)
	}
	var preserved models.AccessKeyCostLimitState
	if err := fixture.db.First(&preserved, fiveHours.ID).Error; err != nil {
		t.Fatalf("load preserved state: %v", err)
	}
	if preserved.UsedNanoUSD != 15_000_000_000 || preserved.RuleRevision != fiveHours.RuleRevision ||
		preserved.WindowGeneration != 2 {
		t.Fatalf("amount update reset state = %#v", preserved)
	}

	// Deleting removes exactly one rule and its state.
	deleted := serveAccessKeyCostLimitRequest(t, engine, http.MethodDelete, rulePath(day.ID), "", "")
	if deleted.Code != http.StatusOK || len(decodeRules(deleted)) != 2 {
		t.Fatalf("DELETE rule = %d %s", deleted.Code, deleted.Body.String())
	}
	var remaining int64
	if err := fixture.db.Model(&models.AccessKeyCostLimitState{}).Where("rule_id = ?", day.ID).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("deleted rule state still exists")
	}
	if again := serveAccessKeyCostLimitRequest(t, engine, http.MethodDelete, rulePath(day.ID), "", ""); again.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing rule = %d %s, want 404", again.Code, again.Body.String())
	}

	// Creating adds exactly one rule with fresh state.
	createdRule := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost,
		fmt.Sprintf("/api/access-keys/%d/cost-limits", accessKeyID),
		`{"kind":"periodic","limit_usd":"40","period_seconds":36000}`, "")
	if createdRule.Code != http.StatusOK || len(decodeRules(createdRule)) != 3 {
		t.Fatalf("POST rule = %d %s", createdRule.Code, createdRule.Body.String())
	}

	// Changing the period starts a new revision with no usage.
	period := serveAccessKeyCostLimitRequest(t, engine, http.MethodPut, rulePath(fiveHours.ID),
		`{"kind":"periodic","limit_usd":"25","period_seconds":21600}`, "")
	if period.Code != http.StatusOK {
		t.Fatalf("PUT rule period = %d %s, want 200", period.Code, period.Body.String())
	}
	var reset models.AccessKeyCostLimitState
	if err := fixture.db.First(&reset, fiveHours.ID).Error; err != nil {
		t.Fatalf("load reset state: %v", err)
	}
	if reset.RuleRevision != fiveHours.RuleRevision+1 || reset.UsedNanoUSD != 0 ||
		reset.WindowStartedAtMS != nil || reset.WindowEndsAtMS != nil || reset.WindowGeneration != 0 {
		t.Fatalf("period update state = %#v, want reset next revision", reset)
	}
	if final := loadAccessKeyCostLimitRules(t, fixture, accessKeyID); len(final) != 3 || final[0].ID != total.ID {
		t.Fatalf("final rules = %#v", final)
	}

	// The AccessKey update no longer accepts rule definitions.
	legacy := serveAccessKeyCostLimitRequest(t, engine, http.MethodPut,
		fmt.Sprintf("/api/access-keys/%d", accessKeyID),
		`{"cost_limit_rules":[{"kind":"total","limit_usd":"1"}]}`, "")
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("PUT access key with rules = %d %s, want 400", legacy.Code, legacy.Body.String())
	}
}

func TestAccessKeyCostLimitRulesResetOnlySelectedRules(t *testing.T) {
	t.Parallel()
	fixture, engine := newAccessKeyCostLimitHTTPFixture(t)
	created := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost, "/api/access-keys", `{
		"name":"resettable",
		"cost_limit_rules":[
			{"kind":"total","limit_usd":"100"},
			{"kind":"periodic","limit_usd":"20","period_seconds":18000},
			{"kind":"periodic","limit_usd":"30","period_seconds":86400}
		]
	}`, "00000000-0000-4000-8000-000000009004")
	if created.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", created.Code, created.Body.String())
	}
	var envelope struct {
		Data AccessKeyCreateResult `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	rules := envelope.Data.CostLimitRules
	if len(rules) != 3 {
		t.Fatalf("created rules = %#v", rules)
	}

	now := time.Unix(1_787_184_000, 0).UTC()
	ticket, decision := fixture.accessQuota.Admit(envelope.Data.ID, now)
	if !decision.Allowed {
		t.Fatalf("Admit() = %#v", decision)
	}
	fixture.accessQuota.Complete(ticket, 15_000_000_000)

	response := serveAccessKeyCostLimitRequest(
		t,
		engine,
		http.MethodPost,
		fmt.Sprintf("/api/access-keys/%d/cost-limits/reset", envelope.Data.ID),
		fmt.Sprintf(`{"rule_ids":[%d,%d]}`, rules[0].ID, rules[1].ID),
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("POST reset = %d %s, want 200", response.Code, response.Body.String())
	}

	view := fixture.accessQuota.Snapshot(envelope.Data.ID, now.Add(time.Minute))
	if len(view.Rules) != 3 {
		t.Fatalf("runtime rules = %#v", view.Rules)
	}
	if view.Rules[0].ID != rules[0].ID || view.Rules[0].UsedNanoUSD != 0 ||
		view.Rules[0].Status != accessquota.RuleStatusAvailable {
		t.Fatalf("reset total runtime = %#v", view.Rules[0])
	}
	if view.Rules[1].ID != rules[1].ID || view.Rules[1].UsedNanoUSD != 0 ||
		view.Rules[1].Status != accessquota.RuleStatusInactive ||
		view.Rules[1].WindowStartedAtMS != nil || view.Rules[1].WindowEndsAtMS != nil {
		t.Fatalf("reset periodic runtime = %#v", view.Rules[1])
	}
	if view.Rules[2].ID != rules[2].ID || view.Rules[2].UsedNanoUSD != 15_000_000_000 ||
		view.Rules[2].Status != accessquota.RuleStatusAvailable ||
		view.Rules[2].WindowStartedAtMS == nil || view.Rules[2].WindowEndsAtMS == nil {
		t.Fatalf("unselected periodic runtime = %#v", view.Rules[2])
	}

	stored := loadAccessKeyCostLimitRules(t, fixture, envelope.Data.ID)
	for index, rule := range stored {
		wantRevision := uint64(1)
		if index < 2 {
			wantRevision = 2
		}
		if rule.RuleRevision != wantRevision {
			t.Fatalf("rule %d revision = %d, want %d", rule.ID, rule.RuleRevision, wantRevision)
		}
		var state models.AccessKeyCostLimitState
		if err := fixture.db.First(&state, rule.ID).Error; err != nil {
			t.Fatal(err)
		}
		if index < 2 && (state.RuleRevision != 2 || state.UsedNanoUSD != 0 ||
			state.WindowStartedAtMS != nil || state.WindowEndsAtMS != nil ||
			state.WindowGeneration != 0 || state.SnapshotVersion != 1) {
			t.Fatalf("reset state for rule %d = %#v", rule.ID, state)
		}
	}
}

func TestAccessKeyCostLimitRulesResetRejectsInvalidSelection(t *testing.T) {
	t.Parallel()
	_, engine := newAccessKeyCostLimitHTTPFixture(t)
	created := serveAccessKeyCostLimitRequest(
		t,
		engine,
		http.MethodPost,
		"/api/access-keys",
		`{"name":"limited","cost_limit_rules":[{"kind":"total","limit_usd":"10"}]}`,
		"00000000-0000-4000-8000-000000009005",
	)
	if created.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", created.Code, created.Body.String())
	}
	var envelope struct {
		Data AccessKeyCreateResult `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{"rule_ids":[]}`, `{"rule_ids":[1,1]}`, `{"rule_ids":[999999]}`} {
		response := serveAccessKeyCostLimitRequest(
			t,
			engine,
			http.MethodPost,
			fmt.Sprintf("/api/access-keys/%d/cost-limits/reset", envelope.Data.ID),
			body,
			"",
		)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("POST reset %s = %d %s, want 400", body, response.Code, response.Body.String())
		}
	}
}

func TestAccessKeyCostLimitRulesAreValidatedAndIdempotent(t *testing.T) {
	t.Parallel()
	fixture, engine := newAccessKeyCostLimitHTTPFixture(t)
	const idempotencyKey = "00000000-0000-4000-8000-000000009002"
	payload := `{"name":"idempotent","cost_limit_rules":[{"kind":"total","limit_usd":"10"}]}`
	first := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost, "/api/access-keys", payload, idempotencyKey)
	second := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost, "/api/access-keys", payload, idempotencyKey)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("idempotent POST = %d/%d, bodies=%s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var keyCount, ruleCount, stateCount int64
	if err := fixture.db.Model(&models.AccessKey{}).Count(&keyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&models.AccessKeyCostLimitRule{}).Count(&ruleCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&models.AccessKeyCostLimitState{}).Count(&stateCount).Error; err != nil {
		t.Fatal(err)
	}
	if keyCount != 1 || ruleCount != 1 || stateCount != 1 {
		t.Fatalf("idempotent counts = key:%d rule:%d state:%d", keyCount, ruleCount, stateCount)
	}

	invalid := []string{
		`{"name":"zero","cost_limit_rules":[{"kind":"total","limit_usd":"0"}]}`,
		`{"name":"duplicate","cost_limit_rules":[{"kind":"periodic","limit_usd":"1","period_seconds":300},{"kind":"periodic","limit_usd":"2","period_seconds":300}]}`,
		`{"name":"null","cost_limit_rules":null}`,
	}
	for index, body := range invalid {
		response := serveAccessKeyCostLimitRequest(
			t, engine, http.MethodPost, "/api/access-keys", body,
			fmt.Sprintf("00000000-0000-4000-8000-%012d", 9100+index),
		)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid payload %d = %d %s, want 400", index, response.Code, response.Body.String())
		}
	}
}

func TestAccessKeyCostLimitRuleSubresourceRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	fixture, engine := newAccessKeyCostLimitHTTPFixture(t)
	create := func(name, rules, idempotencyKey string) AccessKeyCreateResult {
		t.Helper()
		response := serveAccessKeyCostLimitRequest(t, engine, http.MethodPost, "/api/access-keys",
			fmt.Sprintf(`{"name":%q,"key":"%s-access-key-value","cost_limit_rules":%s}`, name, name, rules), idempotencyKey)
		if response.Code != http.StatusOK {
			t.Fatalf("POST = %d %s, want 200", response.Code, response.Body.String())
		}
		var envelope struct {
			Data AccessKeyCreateResult `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	owner := create("owner", `[{"kind":"total","limit_usd":"10"},{"kind":"periodic","limit_usd":"1","period_seconds":300},{"kind":"periodic","limit_usd":"2","period_seconds":600}]`,
		"00000000-0000-4000-8000-000000009003")
	other := create("other", `[{"kind":"total","limit_usd":"10"}]`, "00000000-0000-4000-8000-000000009006")
	total, fiveMinutes := owner.CostLimitRules[0], owner.CostLimitRules[1]
	base := fmt.Sprintf("/api/access-keys/%d/cost-limits", owner.ID)

	for _, test := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"second total rule", http.MethodPost, base, `{"kind":"total","limit_usd":"5"}`, http.StatusBadRequest},
		{"duplicate period", http.MethodPost, base, `{"kind":"periodic","limit_usd":"5","period_seconds":600}`, http.StatusBadRequest},
		{"client supplied id", http.MethodPost, base, `{"id":1,"kind":"periodic","limit_usd":"5","period_seconds":900}`, http.StatusBadRequest},
		{"zero limit", http.MethodPost, base, `{"kind":"periodic","limit_usd":"0","period_seconds":900}`, http.StatusBadRequest},
		{"kind change", http.MethodPut, fmt.Sprintf("%s/%d", base, total.ID), `{"kind":"periodic","limit_usd":"10","period_seconds":900}`, http.StatusBadRequest},
		{"period taken by sibling", http.MethodPut, fmt.Sprintf("%s/%d", base, fiveMinutes.ID), `{"kind":"periodic","limit_usd":"1","period_seconds":600}`, http.StatusBadRequest},
		{"rule of another key", http.MethodPut, fmt.Sprintf("%s/%d", base, other.CostLimitRules[0].ID), `{"kind":"total","limit_usd":"1"}`, http.StatusNotFound},
		{"delete rule of another key", http.MethodDelete, fmt.Sprintf("%s/%d", base, other.CostLimitRules[0].ID), "", http.StatusNotFound},
		{"missing access key", http.MethodPost, "/api/access-keys/999999/cost-limits", `{"kind":"total","limit_usd":"1"}`, http.StatusNotFound},
	} {
		response := serveAccessKeyCostLimitRequest(t, engine, test.method, test.path, test.body, "")
		if response.Code != test.want {
			t.Fatalf("%s = %d %s, want %d", test.name, response.Code, response.Body.String(), test.want)
		}
	}
	stored := loadAccessKeyCostLimitRules(t, fixture, owner.ID)
	if len(stored) != 3 || stored[0].Kind != models.AccessKeyCostLimitKindTotal ||
		stored[1].PeriodSeconds != 300 || stored[2].PeriodSeconds != 600 {
		t.Fatalf("stored rules after rejected changes = %#v", stored)
	}
	for _, rule := range stored {
		if rule.RuleRevision != 1 {
			t.Fatalf("rejected change advanced rule %d revision to %d", rule.ID, rule.RuleRevision)
		}
	}
}

func newAccessKeyCostLimitHTTPFixture(t *testing.T) (serviceFixture, *gin.Engine) {
	t.Helper()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	fixture.service.random = bytes.NewReader(bytes.Repeat([]byte{0x11}, 64))
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "test-auth-key"}, fixture.service).RegisterRoutes(engine)
	return fixture, engine
}

func serveAccessKeyCostLimitRequest(
	t *testing.T,
	engine *gin.Engine,
	method, path, payload, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(payload))
	request.Header.Set("Authorization", "Bearer test-auth-key")
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	engine.ServeHTTP(response, request)
	return response
}

func loadAccessKeyCostLimitRules(
	t *testing.T,
	fixture serviceFixture,
	accessKeyID uint,
) []models.AccessKeyCostLimitRule {
	t.Helper()
	var rules []models.AccessKeyCostLimitRule
	if err := fixture.db.Where("access_key_id = ?", accessKeyID).
		Order("CASE WHEN kind = 'total' THEN 0 ELSE 1 END ASC, period_seconds ASC, id ASC").Find(&rules).Error; err != nil {
		t.Fatalf("load cost limit rules: %v", err)
	}
	return rules
}
