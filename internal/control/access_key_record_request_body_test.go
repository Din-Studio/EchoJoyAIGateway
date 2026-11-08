package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

func setRecordingDefault(t *testing.T, fixture serviceFixture, raw string) SettingsResponse {
	t.Helper()
	result, err := fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingDefaultAccessKeyRecordRequestBody: json.RawMessage(raw),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAccessKeyRecordingDefaultSettingsAndStaleReplicaCreation(t *testing.T) {
	pair := newClusterPair(t)
	defaults, err := pair.a.service.GetSettings(t.Context())
	if err != nil || defaults.Values.DefaultAccessKeyRecordRequestBody {
		t.Fatalf("defaults = %+v, %v", defaults.Values, err)
	}
	old, err := pair.a.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "old", Note: "preserve note"})
	if err != nil {
		t.Fatal(err)
	}
	updated := setRecordingDefault(t, pair.a, "true")
	if !updated.Values.DefaultAccessKeyRecordRequestBody || !slices.Contains(updated.Overrides, state.SettingDefaultAccessKeyRecordRequestBody) {
		t.Fatalf("updated settings = %+v", updated)
	}
	assertRequestBodyRecording(t, pair.a, old.ID, false)
	if pair.b.manager.Current().Settings.DefaultAccessKeyRecordRequestBody {
		t.Fatal("replica B should still have stale defaults")
	}
	// Both creation paths must read PG, not B's stale snapshot.
	for index, idempotent := range []bool{false, true} {
		if _, err := pair.b.service.reloadCommittedConfig(t.Context()); err != nil {
			t.Fatal(err)
		}
		stale := pair.b.manager.Current()
		setRecordingDefault(t, pair.a, "false")
		if !stale.Settings.DefaultAccessKeyRecordRequestBody {
			t.Fatal("expected stale true default")
		}
		for _, explicit := range []*bool{nil, new(false), new(true)} {
			request := AccessKeyCreateRequest{Name: fmt.Sprintf("created-%d-%v", index, explicit), RecordRequestBody: explicit}
			var result AccessKeyCreateResult
			var err error
			if idempotent {
				result, err = pair.b.service.CreateAccessKeyIdempotent(t.Context(), fmt.Sprintf("00000000-0000-4000-8000-%012d", testIdempotencySequence.Add(1)), request)
			} else {
				result, err = pair.b.service.CreateAccessKey(t.Context(), request)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := explicit != nil && *explicit
			if result.RecordRequestBody != want {
				t.Fatalf("create recording = %v, want %v", result.RecordRequestBody, want)
			}
			assertRequestBodyRecording(t, pair.b, result.ID, want)
		}
		setRecordingDefault(t, pair.a, "true")
	}
	pair.reloadB(t)
	if !pair.b.manager.Current().Settings.DefaultAccessKeyRecordRequestBody {
		t.Fatal("default did not propagate")
	}
	inherited, err := pair.b.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "inherit true"})
	if err != nil || !inherited.RecordRequestBody {
		t.Fatalf("inherit true = %+v, %v", inherited, err)
	}
	reset := setRecordingDefault(t, pair.b, "null")
	if reset.Values.DefaultAccessKeyRecordRequestBody || slices.Contains(reset.Overrides, state.SettingDefaultAccessKeyRecordRequestBody) {
		t.Fatalf("reset settings = %+v", reset)
	}
	assertRequestBodyRecording(t, pair.b, inherited.ID, true)
	for _, raw := range []string{`"true"`, `1`, `{}`, `[]`} {
		_, err := pair.b.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
			state.SettingDefaultAccessKeyRecordRequestBody: json.RawMessage(raw),
		}})
		if !errors.Is(err, app_errors.ErrValidation) {
			t.Fatalf("invalid setting %s: %v", raw, err)
		}
	}
}

func TestAccessKeyRecordingDefaultHTTPValuesAndCreation(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	engine := newAccessKeyLifecycleEngine(t, fixture)
	updated := serveLocalizedSettingsRequest(t, engine, http.MethodPut, authTestKey, "en-US",
		`{"settings":{"default_access_key_record_request_body":true}}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update default = %d %s", updated.Code, updated.Body)
	}
	var envelope struct {
		Data struct {
			Values map[string]json.RawMessage `json:"values"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertJSONRawEqual(t, envelope.Data.Values[state.SettingDefaultAccessKeyRecordRequestBody], "true")
	for index, test := range []struct{ body, want string }{
		{`{"name":"inherited"}`, "true"},
		{`{"name":"null inherits","record_request_body":null}`, "true"},
		{`{"name":"explicit false","record_request_body":false}`, "false"},
		{`{"name":"explicit true","record_request_body":true}`, "true"},
	} {
		created := serveAccessKeyLifecycleRequest(t, engine, http.MethodPost, "/api/access-keys", test.body,
			fmt.Sprintf("00000000-0000-4000-8000-%012d", 6500+index))
		if created.Code != http.StatusOK {
			t.Fatalf("create = %d %s", created.Code, created.Body)
		}
		assertJSONRawEqual(t, decodeAccessKeyLifecycleData(t, created)["record_request_body"], test.want)
	}
	// Turning existing keys off must leave the creation default on.
	if _, err := fixture.service.SetAccessKeyRecordRequestBody(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	current := serveLocalizedSettingsRequest(t, engine, http.MethodGet, authTestKey, "en-US", "")
	if current.Code != http.StatusOK {
		t.Fatalf("get default = %d %s", current.Code, current.Body)
	}
	if err := json.Unmarshal(current.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertJSONRawEqual(t, envelope.Data.Values[state.SettingDefaultAccessKeyRecordRequestBody], "true")
}

func TestAccessKeyRecordingIdempotencyDistinguishesOmittedFalseTrue(t *testing.T) {
	fixture := newServiceFixture(t)
	setRecordingDefault(t, fixture, "true")
	for index, explicit := range []*bool{nil, new(false), new(true)} {
		key := fmt.Sprintf("00000000-0000-4000-8000-%012d", 6496+index)
		request := AccessKeyCreateRequest{Name: "idempotent", RecordRequestBody: explicit}
		created, err := fixture.service.CreateAccessKeyIdempotent(t.Context(), key, request)
		if err != nil {
			t.Fatal(err)
		}
		want := explicit == nil || *explicit
		if created.RecordRequestBody != want {
			t.Fatalf("created recording = %v, want %v", created.RecordRequestBody, want)
		}
		setRecordingDefault(t, fixture, "false")
		if _, err := fixture.service.SetAccessKeyRecordRequestBody(t.Context(), !want); err != nil {
			t.Fatal(err)
		}
		replayed, err := fixture.service.CreateAccessKeyIdempotent(t.Context(), key, request)
		if err != nil || !replayed.Replayed || replayed.ID != created.ID || replayed.Key != "" || replayed.RecordRequestBody != want {
			t.Fatalf("replayed = %+v, %v", replayed, err)
		}
		for _, other := range []*bool{nil, new(false), new(true)} {
			if (explicit == nil && other == nil) || (explicit != nil && other != nil && *explicit == *other) {
				continue
			}
			request.RecordRequestBody = other
			_, err := fixture.service.CreateAccessKeyIdempotent(t.Context(), key, request)
			var apiErr *app_errors.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != app_errors.ErrIdempotencyKeyReused.Code {
				t.Fatalf("changed recording must conflict: %v", err)
			}
		}
		setRecordingDefault(t, fixture, "true")
	}
}

func TestAccessKeyRecordingLegacyReplayDoesNotApplyNewDefault(t *testing.T) {
	fixture := newServiceFixture(t)
	const key = "00000000-0000-4000-8000-000000006499"
	request := AccessKeyCreateRequest{Name: "legacy"}
	created, err := fixture.service.CreateAccessKeyIdempotent(t.Context(), key, request)
	if err != nil {
		t.Fatal(err)
	}
	var operation models.ControlOperation
	if err := fixture.db.Where("idempotency_key = ?", key).Take(&operation).Error; err != nil {
		t.Fatal(err)
	}
	// Old results lack the additive field. The pre-feature digest is separately
	// pinned by TestAccessKeyLifecycleDefaultCreatePreservesV1DigestAndNullEquivalence.
	var oldResult map[string]json.RawMessage
	if err := json.Unmarshal(operation.CanonicalResult, &oldResult); err != nil {
		t.Fatal(err)
	}
	delete(oldResult, "record_request_body")
	encoded, err := json.Marshal(oldResult)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&operation).Update("canonical_result", encoded).Error; err != nil {
		t.Fatal(err)
	}
	setRecordingDefault(t, fixture, "true")
	replayed, err := fixture.service.CreateAccessKeyIdempotent(t.Context(), key, request)
	if err != nil || !replayed.Replayed || replayed.RecordRequestBody || replayed.ID != created.ID {
		t.Fatalf("legacy replay = %+v, %v", replayed, err)
	}
	assertRequestBodyRecording(t, fixture, created.ID, false)
}

func TestAccessKeyRecordingBulkAllKeysTransactionAndPropagation(t *testing.T) {
	pair := newClusterPair(t)
	var keys []AccessKeyCreateResult
	for index := range 3 {
		key, err := pair.a.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: fmt.Sprintf("key-%d", index), Note: "keep"})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := pair.a.db.Model(&models.AccessKey{}).Where("id = ?", keys[1].ID).Update("status", "disabled").Error; err != nil {
		t.Fatal(err)
	}
	if err := pair.a.db.Model(&models.AccessKey{}).Where("id = ?", keys[2].ID).Update("expires_at_ms", 1).Error; err != nil {
		t.Fatal(err)
	}
	publisher := pair.a.service.clusterEvents.(*recordingConfigEventPublisher)
	before, err := readClusterConfigRevision(t.Context(), pair.a.db)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the revision write after the bulk UPDATE: every row must roll back.
	constraint := fmt.Sprintf("ALTER TABLE system_settings ADD CONSTRAINT reject_recording_revision CHECK (key <> '%s' OR value <> '%d')", clusterConfigRevisionKey, before+1)
	if err := pair.a.db.Exec(constraint).Error; err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(publisher.published())
	if _, err := pair.a.service.SetAccessKeyRecordRequestBody(t.Context(), true); err == nil {
		t.Fatal("expected revision failure")
	}
	for _, key := range keys {
		assertRequestBodyRecording(t, pair.a, key.ID, false)
	}
	afterFailure, err := readClusterConfigRevision(t.Context(), pair.a.db)
	if err != nil || afterFailure != before || len(publisher.published()) != beforeEvents {
		t.Fatalf("failed transaction published or advanced revision: %d, %v", afterFailure, err)
	}
	if err := pair.a.db.Exec("ALTER TABLE system_settings DROP CONSTRAINT reject_recording_revision").Error; err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		result, err := pair.a.service.SetAccessKeyRecordRequestBody(t.Context(), enabled)
		if err != nil || result.UpdatedCount != 3 {
			t.Fatalf("bulk = %+v, %v", result, err)
		}
		before++
		changes := publisher.published()
		if len(changes) != beforeEvents+1 || changes[len(changes)-1].Revision != before {
			t.Fatalf("expected one revision/event: %+v", changes)
		}
		beforeEvents++
		if revision := pair.reloadB(t); revision != before {
			t.Fatalf("remote revision = %d, want %d", revision, before)
		}
		for _, key := range keys {
			assertRequestBodyRecording(t, pair.a, key.ID, enabled)
			assertRequestBodyRecording(t, pair.b, key.ID, enabled)
			metadata, err := pair.b.service.GetAccessKey(t.Context(), key.ID)
			if err != nil || metadata.Note != "keep" || metadata.RecordRequestBody != enabled {
				t.Fatalf("metadata = %+v, %v", metadata, err)
			}
		}
	}
	settings, err := pair.a.service.GetSettings(t.Context())
	if err != nil || settings.Values.DefaultAccessKeyRecordRequestBody || slices.Contains(settings.Overrides, state.SettingDefaultAccessKeyRecordRequestBody) {
		t.Fatalf("bulk changed default: %+v, %v", settings, err)
	}
}

func TestAccessKeyRecordingBulkHTTPAuthorizationValidationAndAudit(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	engine := newAccessKeyLifecycleEngine(t, fixture)
	const path = "/api/access-keys/record-request-body"
	for _, test := range []struct {
		name, token, body string
		status            int
	}{
		{"unauthenticated", "", `{"record_request_body":true}`, http.StatusUnauthorized},
		{"access key", key.Key, `{"record_request_body":true}`, http.StatusForbidden},
		{"missing", authTestKey, `{}`, http.StatusBadRequest},
		{"null", authTestKey, `{"record_request_body":null}`, http.StatusBadRequest},
		{"wrong type", authTestKey, `{"record_request_body":"true"}`, http.StatusBadRequest},
		{"unknown field", authTestKey, `{"record_request_body":true,"ids":[]}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d: %s", response.Code, response.Body)
			}
			assertRequestBodyRecording(t, fixture, key.ID, false)
		})
	}
	response := serveAccessKeyLifecycleRequest(t, engine, http.MethodPut, path, `{"record_request_body":true}`, "")
	if response.Code != http.StatusOK {
		t.Fatalf("bulk = %d: %s", response.Code, response.Body)
	}
	assertJSONRawEqual(t, decodeAccessKeyLifecycleData(t, response)["updated_count"], "1")
	assertRequestBodyRecording(t, fixture, key.ID, true)
	event, status := runMutationAuditRequest(t, fixture, mutationAuditRequest{method: http.MethodPut, path: path, body: `{"record_request_body":false}`})
	assertMutationEvent(t, event, "access_keys_record_request_body", "access_key", "all", "192.0.2.1", "succeeded", status, "", "info")
	if status != http.StatusOK {
		t.Fatalf("audit request status = %d", status)
	}
}
