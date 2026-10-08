package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/storage/models"
)

// TestExternalDatabaseReservedIdentifierQueries verifies that runtime query
// scopes quote table and column names which are reserved by supported drivers.
func TestExternalDatabaseReservedIdentifierQueries(t *testing.T) {
	dsn := externalDatabaseDSN(t)
	db := openControlTestDBWithDSN(t, dsn)

	if err := homeCredentialRowsScope(db).Find(&[]homeCredentialRow{}).Error; err != nil {
		t.Fatalf("query home credentials: %v", err)
	}
	var setting models.SystemSetting
	if err := globalProxyConfigScope(db).Take(&setting).Error; err != nil &&
		!errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("query global proxy config: %v", err)
	}
}

// TestExternalDatabaseAccessKeyCostLimitPeriodPermutation verifies that the
// retained-rule two-phase period move obeys the real PostgreSQL unique
// index while preserving IDs and resetting each changed revision.
func TestExternalDatabaseAccessKeyCostLimitPeriodPermutation(t *testing.T) {
	dsn := externalDatabaseDSN(t)
	assertAccessKeyCostLimitPeriodPermutation(
		t,
		newExternalServiceFixture(t, dsn),
		[]int64{300, 600, 900},
		[]int64{600, 900, 300},
	)
}

// TestExternalDatabaseGroupPriceReconciliation verifies the actual control
// write chain on each supported external driver. Two Groups can reference one
// global model price, and removing the final reference cleans only the
// automatic row.
func TestExternalDatabaseGroupPriceReconciliation(t *testing.T) {
	dsn := externalDatabaseDSN(t)

	fixture := newExternalServiceFixture(t, dsn)
	suffix := time.Now().UnixNano()
	modelID := fmt.Sprintf("external-control-model-%d", suffix)
	create := func(name, upstreamURL string) GroupCreateResult {
		result, err := fixture.service.CreateGroup(t.Context(), GroupCreateRequest{
			Name:      &name,
			ChannelID: channel.OpenAICompatible,
			Params:    json.RawMessage(`{"base_url":"` + upstreamURL + `"}`),
			Models: optionalGroupModels{Set: true, Values: []GroupModel{{
				ID: modelID,
			}}},
			Credentials: fmt.Sprintf("sk-external-control-%d", suffix), ConnectionType: "api_key",
		})
		if err != nil {
			t.Fatalf("CreateGroup(%q) error = %v", name, err)
		}
		return result
	}

	first := create(
		fmt.Sprintf("external-control-first-%d", suffix),
		fmt.Sprintf("https://external-control-first-%d.example.com/v1", suffix),
	)
	assertExternalAutomaticModelPriceCount(t, fixture, modelID, 1)

	second := create(
		fmt.Sprintf("external-control-second-%d", suffix),
		fmt.Sprintf("https://external-control-second-%d.example.com/v1", suffix),
	)
	assertExternalAutomaticModelPriceCount(t, fixture, modelID, 1)

	empty := GroupModelsUpdateRequest{Models: optionalGroupModels{Set: true, Values: []GroupModel{}}}
	if _, err := fixture.service.UpdateGroupModels(t.Context(), first.GroupID, empty); err != nil {
		t.Fatalf("UpdateGroupModels(first) error = %v", err)
	}
	assertExternalAutomaticModelPriceCount(t, fixture, modelID, 1)
	if _, err := fixture.service.UpdateGroupModels(t.Context(), second.GroupID, empty); err != nil {
		t.Fatalf("UpdateGroupModels(second) error = %v", err)
	}
	assertExternalAutomaticModelPriceCount(t, fixture, modelID, 0)
}

func assertExternalAutomaticModelPriceCount(
	t *testing.T,
	fixture serviceFixture,
	modelID string,
	want int64,
) {
	t.Helper()
	var count int64
	if err := fixture.db.Model(&models.ModelPrice{}).
		Where("model_id = ? AND is_manual = ?", modelID, false).
		Count(&count).Error; err != nil {
		t.Fatalf("count automatic model price: %v", err)
	}
	if count != want {
		t.Fatalf("automatic price count for %q = %d, want %d", modelID, count, want)
	}
}
