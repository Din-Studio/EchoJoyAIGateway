package control

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpt-load/internal/catalog"
	"gpt-load/internal/channel"
	"gpt-load/internal/storage/models"
)

func TestClientCatalogReplicaWritesPreserveIndependentFields(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	createPriceTestGroup(t, first.db, models.Group{
		Name: "replica-catalog", ChannelID: string(channel.OpenAI), Params: models.JSON(`{}`),
		Models: models.JSON(`[{"id":"gpt-6"},{"id":"gpt-5"}]`), Overrides: models.JSON(`{}`), Enabled: true,
	})
	mustPublishClientModelSnapshot(t, first)
	mustPublishClientModelSnapshot(t, second)
	if _, err := first.service.UpdateClientCatalog(t.Context(), ClientCatalogRequest{Models: []string{"gpt-5"}}); err != nil {
		t.Fatal(err)
	}
	if len(second.manager.Current().ClientModelOverrides) != 0 {
		t.Fatal("second replica must start with a stale snapshot")
	}
	description := "from stale replica"
	if _, err := second.service.UpdateClientModelProfile(t.Context(), ClientModelProfileUpdateRequest{
		ClientModel: "gpt-5", Overrides: &catalog.ClientModelOverrides{Description: &description},
	}); err != nil {
		t.Fatal(err)
	}
	assert := func(wantDescription string) {
		t.Helper()
		input := mustBuildCompileInput(t, first.db)
		profile := input.ClientModelOverrides["gpt-5"]
		hidden := input.ClientModelOverrides["gpt-6"].CatalogEnabled
		if profile.CatalogOrder == nil || *profile.CatalogOrder != 0 || hidden == nil || *hidden ||
			profile.Description == nil || *profile.Description != wantDescription {
			t.Fatalf("directory/profile fields lost: %#v, hidden=%v", profile, hidden)
		}
	}
	assert(description)
	// Each service owns a different writeMu. Only the PG lock serializes their
	// read-modify-write operations; both may still hold an older local snapshot.
	for index := range 8 {
		description = fmt.Sprintf("concurrent-%d", index)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := first.service.UpdateClientCatalog(t.Context(), ClientCatalogRequest{Models: []string{"gpt-5"}})
			results <- err
		}()
		go func() {
			<-start
			_, err := second.service.UpdateClientModelProfile(t.Context(), ClientModelProfileUpdateRequest{
				ClientModel: "gpt-5", Overrides: &catalog.ClientModelOverrides{Description: &description},
			})
			results <- err
		}()
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		assert(description)
	}
	if _, err := first.service.UpdateClientCatalog(t.Context(), ClientCatalogRequest{ResetDirectory: true}); err != nil {
		t.Fatal(err)
	}
	profile := mustBuildCompileInput(t, first.db).ClientModelOverrides["gpt-5"]
	if profile.Description == nil || *profile.Description != description || profile.CatalogOrder != nil {
		t.Fatal("directory reset did not preserve committed peer metadata")
	}
}

func TestClientCatalogBothWritersRespectTransactionLock(t *testing.T) {
	fixture := newServiceFixture(t)
	createPriceTestGroup(t, fixture.db, models.Group{
		Name: "locked-catalog", ChannelID: string(channel.OpenAI), Params: models.JSON(`{}`),
		Models: models.JSON(`[{"id":"gpt-6"}]`), Overrides: models.JSON(`{}`), Enabled: true,
	})
	mustPublishClientModelSnapshot(t, fixture)
	before := mustBuildCompileInput(t, fixture.db).ClientModelOverrides
	tx := fixture.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT pg_advisory_xact_lock(817)").Error; err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"catalog", "profile"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			var err error
			if endpoint == "catalog" {
				_, err = fixture.service.UpdateClientCatalog(ctx, ClientCatalogRequest{Models: []string{}})
			} else {
				description := "must not commit"
				_, err = fixture.service.UpdateClientModelProfile(ctx, ClientModelProfileUpdateRequest{
					ClientModel: "gpt-6", Overrides: &catalog.ClientModelOverrides{Description: &description},
				})
			}
			if err == nil || ctx.Err() == nil {
				t.Fatalf("writer bypassed the shared transaction lock: %v", err)
			}
			if after := mustBuildCompileInput(t, fixture.db).ClientModelOverrides; !reflect.DeepEqual(before, after) {
				t.Fatal("blocked writer changed persisted state")
			}
		})
	}
}
