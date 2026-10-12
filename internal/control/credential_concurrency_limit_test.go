package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/channel"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

func TestUpdateCredentialConcurrencyLimitPersistsAndPublishes(t *testing.T) {
	t.Parallel()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	groupID := createGroupForCredentialImport(t, fixture, "sk-concurrency-limit")
	var credential models.Credential
	if err := fixture.db.Where("group_id = ?", groupID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}

	for _, value := range []int{-1, 10001} {
		_, err := fixture.service.UpdateGroupCredential(t.Context(), groupID, credential.ID, CredentialUpdateRequest{
			ConcurrencyLimit: optionalField[int]{Set: true, Value: value},
		})
		if !errors.Is(err, app_errors.ErrValidation) {
			t.Fatalf("concurrency_limit %d error = %v, want validation", value, err)
		}
	}

	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: "test-auth-key"}, fixture.service).RegisterRoutes(engine)
	classicDetail := fmt.Sprintf("/api/groups/%d/credentials/%d", groupID, credential.ID)
	modernDetail := fmt.Sprintf("/api/modern/groups/%d/credentials/%d", groupID, credential.ID)
	for _, test := range []struct {
		name  string
		patch string
		want  int
	}{
		{name: "set", patch: `{"concurrency_limit":5}`, want: 5},
		{name: "unlimited", patch: `{"concurrency_limit":0}`, want: 0},
		{name: "set again", patch: `{"concurrency_limit":10000}`, want: 10000},
		{name: "clear", patch: `{"concurrency_limit":null}`, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveGroupDetailLedgerRoute(t, engine, http.MethodPut, classicDetail, test.patch, "Bearer test-auth-key")
			assertGroupDetailLedgerEnvelope(t, recorder, http.StatusOK, "")
			var classic struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &classic); err != nil {
				t.Fatal(err)
			}
			if _, exists := classic.Data["concurrency_limit"]; exists {
				t.Fatalf("classic response unexpectedly exposes concurrency_limit: %s", recorder.Body.String())
			}

			var stored models.Credential
			if err := fixture.db.Take(&stored, credential.ID).Error; err != nil {
				t.Fatal(err)
			}
			ref, ok := fixture.registry.CredentialRef(credential.ID)
			if stored.ConcurrencyLimit != test.want || !ok || ref.ConcurrencyLimit != test.want {
				t.Fatalf("concurrency_limit db=%d registry=%d (%v), want %d", stored.ConcurrencyLimit, ref.ConcurrencyLimit, ok, test.want)
			}

			recorder = serveGroupDetailLedgerRoute(t, engine, http.MethodGet, modernDetail, "", "Bearer test-auth-key")
			assertGroupDetailLedgerEnvelope(t, recorder, http.StatusOK, "")
			var modern struct {
				Data struct {
					Credential map[string]json.RawMessage `json:"credential"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &modern); err != nil {
				t.Fatal(err)
			}
			if got := string(modern.Data.Credential["concurrency_limit"]); got != fmt.Sprint(test.want) {
				t.Fatalf("modern concurrency_limit = %s, want %d", got, test.want)
			}
		})
	}
}

func TestCreateAndImportStorePerEntryConcurrencyLimit(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	entry := func(key string, limit int) CredentialEntryInput {
		return CredentialEntryInput{Fields: map[string]string{"api_key": key}, ConcurrencyLimit: limit}
	}
	created, err := fixture.service.CreateGroupIdempotent(t.Context(), "00000000-0000-4000-8000-0000000c0001", GroupCreateRequest{
		Name: stringPointer("entry concurrency"), ChannelID: channel.OpenAI, Params: json.RawMessage(`{}`),
		Models: optionalGroupModels{Set: true}, ConnectionType: "api_key",
		Credentials: []CredentialEntryInput{entry("sk-limited", 7), entry("sk-unlimited", 0)},
	})
	if err != nil {
		t.Fatalf("CreateGroupIdempotent() error = %v", err)
	}
	if _, err := fixture.service.ImportGroupCredentialsIdempotent(
		t.Context(), "00000000-0000-4000-8000-0000000c0002", created.GroupID,
		CredentialImportRequest{Credentials: []CredentialEntryInput{entry("sk-imported", 3)}},
	); err != nil {
		t.Fatalf("ImportGroupCredentialsIdempotent() error = %v", err)
	}
	var rows []models.Credential
	if err := fixture.db.Where("group_id = ?", created.GroupID).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := []int{7, 0, 3}
	if len(rows) != len(want) {
		t.Fatalf("stored credentials = %d, want %d", len(rows), len(want))
	}
	for index, row := range rows {
		ref, ok := fixture.registry.CredentialRef(row.ID)
		if row.ConcurrencyLimit != want[index] || !ok || ref.ConcurrencyLimit != want[index] {
			t.Fatalf("credential %d concurrency_limit db=%d registry=%d (%v), want %d",
				index, row.ConcurrencyLimit, ref.ConcurrencyLimit, ok, want[index])
		}
	}

	_, err = fixture.service.ImportGroupCredentialsIdempotent(
		t.Context(), "00000000-0000-4000-8000-0000000c0003", created.GroupID,
		CredentialImportRequest{Credentials: []CredentialEntryInput{entry("sk-ok", 1), entry("sk-too-many", 10001)}},
	)
	var apiErr *app_errors.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != app_errors.ErrValidation.Code {
		t.Fatalf("out-of-range concurrency_limit error = %v, want validation", err)
	}
	if data, ok := apiErr.Data.(credentialValidationData); !ok || data.Entry != 2 || data.Field != "concurrency_limit" {
		t.Fatalf("validation data = %#v, want entry 2 concurrency_limit", apiErr.Data)
	}

	_, err = fixture.service.ImportGroupCredentialsIdempotent(
		t.Context(), "00000000-0000-4000-8000-0000000c0004", created.GroupID,
		CredentialImportRequest{Credentials: []CredentialEntryInput{entry("sk-twice", 5), entry("sk-twice", 10)}},
	)
	if !errors.As(err, &apiErr) || apiErr.Code != app_errors.ErrValidation.Code {
		t.Fatalf("conflicting duplicate limit error = %v, want validation", err)
	}
	if data, ok := apiErr.Data.(credentialValidationData); !ok || data.Entry != 2 || data.Field != "concurrency_limit" ||
		data.ReasonCode != "conflicting_value" {
		t.Fatalf("validation data = %#v, want entry 2 concurrency_limit conflicting_value", apiErr.Data)
	}
}
