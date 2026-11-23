package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/catalog"
	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

func TestModelPriceOppositeReplicaLinksCannotCommitCycle(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	for range 8 {
		rows, _ := priceReferenceFixture(t, first, 2)
		start := make(chan struct{})
		results := make(chan error, 2)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		for index, service := range []*Service{first.service, second.service} {
			go func() {
				<-start
				_, err := service.BatchModelPrices(ctx, ModelPriceBatchRequest{IDs: []uint{rows[index].ID}, Mode: "link", Reference: &ModelPriceReferenceRequest{PriceID: rows[1-index].ID}})
				results <- err
			}()
		}
		close(start)
		left, right := <-results, <-results
		cancel()
		if !((left == nil && errors.Is(right, app_errors.ErrModelPriceReferenceCycle)) || (right == nil && errors.Is(left, app_errors.ErrModelPriceReferenceCycle))) {
			t.Fatalf("opposite links = %v / %v; want one commit and one cycle rejection", left, right)
		}
		var saved []models.ModelPrice
		if err := first.db.Find(&saved).Error; err != nil {
			t.Fatal(err)
		}
		if err := resolveModelPriceReferences(saved); err != nil {
			t.Fatalf("committed invalid graph: %v", err)
		}
	}
}

func TestModelPriceReplicaUpdateAndLinkPropagateCommittedSource(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	rows, _ := priceReferenceFixture(t, first, 3)
	followPrice(t, first, rows[2].ID, rows[1].ID)
	request := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"9"`, "false"))
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := first.service.UpdateModelPrice(t.Context(), rows[0].ID, request)
		results <- err
	}()
	go func() {
		<-start
		_, err := second.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: []uint{rows[1].ID}, Mode: "link", Reference: &ModelPriceReferenceRequest{PriceID: rows[0].ID}})
		results <- err
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []serviceFixture{first, second} {
		if _, err := fixture.service.reloadCommittedConfig(t.Context()); err != nil {
			t.Fatal(err)
		}
		input := int64(9_000_000_000)
		for _, row := range rows {
			assertFollowedPrice(t, fixture, row, &input)
		}
	}
}

func TestModelPriceReplicaLinkAndLastGroupRemovalAreAtomic(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	for range 6 {
		rows, groups := priceReferenceFixture(t, first, 2)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := first.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: []uint{rows[1].ID}, Mode: "link", Reference: &ModelPriceReferenceRequest{PriceID: rows[0].ID}})
			results <- err
		}()
		go func() {
			<-start
			_, err := second.service.UpdateGroupModels(t.Context(), groups[0].ID, GroupModelsUpdateRequest{Models: optionalGroupModels{Set: true, Values: []GroupModel{}}})
			results <- err
		}()
		close(start)
		left, right := <-results, <-results
		// Either the link commits and protects the source, or removal commits and
		// the source is unavailable. Both committing would leave a hidden source.
		if (left == nil) == (right == nil) {
			t.Fatalf("link/removal = %v / %v", left, right)
		}
		for _, err := range []error{left, right} {
			if err == nil {
				continue
			}
			var apiErr *app_errors.APIError
			if !errors.As(err, &apiErr) || (apiErr.Code != app_errors.ErrModelPriceSourceReferenced.Code && apiErr.Code != app_errors.ErrModelPriceSourceUnavailable.Code) {
				t.Fatalf("unexpected race failure: %v", err)
			}
		}
		var saved []models.ModelPrice
		if err := first.db.Order("id").Find(&saved).Error; err != nil {
			t.Fatal(err)
		}
		references, err := loadReferencedPrices(first.db)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateGroupedPriceSources(saved, references); err != nil {
			t.Fatalf("committed unavailable source: %v", err)
		}
	}
}

func TestModelPriceRevisionLockPrecedesEveryMutation(t *testing.T) {
	fixture := newServiceFixture(t)
	rows, groups := priceReferenceFixture(t, fixture, 2)
	request := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"5"`, "false"))
	if _, err := bumpClusterConfigRevision(fixture.db, fixture.service.now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingConfigEventPublisher{}
	fixture.service.clusterEvents = publisher
	tx := fixture.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT key FROM system_settings WHERE key = ? FOR UPDATE", clusterConfigRevisionKey).Error; err != nil {
		t.Fatal(err)
	}
	name := "graph-lock-create"
	create := GroupCreateRequest{Name: &name, ChannelID: channel.OpenAICompatible, ConnectionType: models.ConnectionTypeAPIKey,
		Params: json.RawMessage(`{"base_url":"https://graph-lock.invalid/v1"}`), Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: "new-model"}}}, Credentials: apiKeyEntries("graph-lock-credential")}
	mutations := map[string]func(context.Context) error{
		"callback": func(ctx context.Context) error {
			return fixture.service.withControlTransaction(ctx, func(*gorm.DB) error {
				return errors.New("mutation callback ran before acquiring the revision lock")
			})
		},
		"group-create": func(ctx context.Context) error { _, err := fixture.service.CreateGroup(ctx, create); return err },
		"group-create-idempotent": func(ctx context.Context) error {
			_, err := fixture.service.CreateGroupIdempotent(ctx, "181f1c2f-a801-49db-bf9a-69b90d61ef99", create)
			return err
		},
		"group-channel": func(ctx context.Context) error {
			_, err := fixture.service.UpdateGroupChannel(ctx, groups[0].ID, GroupChannelUpdateRequest{ChannelID: channel.OpenAI})
			return err
		},
		"unified-price": func(ctx context.Context) error {
			_, err := fixture.service.PutClientModelPrice(ctx, ClientModelPriceRequest{ClientModel: "unified", ModelPriceUpdateRequest: request})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := fixture.service.UpdateModelPrice(ctx, rows[0].ID, request)
			return err
		},
		"reset": func(ctx context.Context) error {
			_, err := fixture.service.ResetModelPrice(ctx, rows[0].ID)
			return err
		},
		// Without the revision lock this would return a reference error immediately.
		"delete": func(ctx context.Context) error { return fixture.service.DeleteModelPrice(ctx, rows[0].ID) },
		"batch": func(ctx context.Context) error {
			_, err := fixture.service.BatchModelPrices(ctx, ModelPriceBatchRequest{IDs: []uint{rows[1].ID}, Mode: "link", Reference: &ModelPriceReferenceRequest{PriceID: rows[0].ID}})
			return err
		},
		"catalog": func(ctx context.Context) error { return fixture.service.applyCatalogSnapshot(ctx, &catalog.Snapshot{}) },
		"startup": fixture.service.EnsureInitialState,
		"group-models": func(ctx context.Context) error {
			_, err := fixture.service.UpdateGroupModels(ctx, groups[0].ID, GroupModelsUpdateRequest{Models: optionalGroupModels{Set: true}})
			return err
		},
		"group-delete": func(ctx context.Context) error { return fixture.service.DeleteGroup(ctx, groups[0].ID) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := mutate(ctx)
			if err == nil || ctx.Err() == nil {
				t.Fatalf("mutation did not wait for the revision lock: %v", err)
			}
		})
	}
	if revision, ok := readStoredClusterRevision(t, fixture.db); !ok || revision != "1" {
		t.Fatalf("blocked mutations changed revision: %s", revision)
	}
	if changes := publisher.published(); len(changes) != 0 {
		t.Fatalf("blocked mutations published changes: %+v", changes)
	}
}

func TestModelPriceCommittedPublicationRetainsSourceAndLinkedRules(t *testing.T) {
	fixture := newServiceFixture(t)
	rows, _ := priceReferenceFixture(t, fixture, 2)
	followPrice(t, fixture, rows[1].ID, rows[0].ID)
	request := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"8"`, "false"))
	if _, err := fixture.service.UpdateModelPrice(t.Context(), rows[0].ID, request); err != nil {
		t.Fatal(err)
	}
	current := fixture.priceRuntime.Load()
	input := int64(8_000_000_000)
	assertFollowedPrice(t, fixture, rows[1], &input)
	if _, exists := current.Lookup(pricing.Identity{ChannelID: rows[0].ChannelID, ModelID: rows[0].ModelID}); !exists {
		t.Fatal("source rule missing")
	}
}

// A post-commit event hook gives the peer a deterministic publication window.
type priceCommitEventPublisher func(context.Context, cluster.ConfigChange) error

func (publish priceCommitEventPublisher) Publish(ctx context.Context, change cluster.ConfigChange) error {
	return publish(ctx, change)
}

func TestGroupCommitRecoversAfterTransientPricePublicationFailure(t *testing.T) {
	fixture := newServiceFixture(t)
	committed, failed := false, false
	fixture.service.clusterEvents = priceCommitEventPublisher(func(ctx context.Context, event cluster.ConfigChange) error {
		// An independent reader must already see the revision announced to peers.
		revision, err := readClusterConfigRevision(ctx, fixture.db)
		if err != nil || revision != event.Revision || revision != 1 {
			t.Errorf("published revision %d before commit: stored %d, %v", event.Revision, revision, err)
		}
		committed = true
		return nil
	})
	const callback = "test:fail_first_committed_price_read"
	if err := fixture.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if committed && !failed && tx.Statement.Table == "model_prices" {
			failed = true
			tx.AddError(errors.New("transient post-commit price read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = fixture.db.Callback().Query().Remove(callback) })
	name := "recover-committed-group"
	_, err := fixture.service.CreateGroup(t.Context(), GroupCreateRequest{
		Name: &name, ChannelID: channel.OpenAI, ConnectionType: models.ConnectionTypeAPIKey,
		Params: json.RawMessage(`{}`), Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-4o"}}},
		Credentials: apiKeyEntries("recovery-test-key"),
	})
	if !failed || !errors.Is(err, app_errors.ErrDatabase) {
		t.Fatalf("CreateGroup: injected=%v, err=%v; want original publication error", failed, err)
	}
	var group models.Group
	if err := fixture.db.Where("name = ?", name).Take(&group).Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := fixture.manager.Current().Groups[group.ID]; !ok {
		t.Fatalf("missing committed group %d in recovered snapshot", group.ID)
	}
	var credential models.Credential
	if err := fixture.db.Where("group_id = ?", group.ID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if encrypted, ok := fixture.registry.EncryptedCredentialData(credential.ID); !ok || encrypted != credential.Data {
		t.Fatalf("registry did not recover committed credential %d", credential.ID)
	}
	if _, ok := fixture.priceRuntime.Load().Lookup(pricing.Identity{ChannelID: string(channel.OpenAI), ModelID: "gpt-4o"}); !ok {
		t.Fatal("price table did not recover the committed model")
	}
}

func TestModelPriceDelayedWriterPublishesLatestPeerPricesAndUnifiedRules(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	rows, _ := priceReferenceFixture(t, first, 2)
	followPrice(t, first, rows[1].ID, rows[0].ID)
	committed, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first.service.clusterEvents = priceCommitEventPublisher(func(ctx context.Context, _ cluster.ConfigChange) error {
		close(committed)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	request := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"4"`, "false"))
	done := make(chan error, 1)
	go func() { _, err := first.service.UpdateModelPrice(t.Context(), rows[0].ID, request); done <- err }()
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not commit")
	}
	request = mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"6"`, "false"))
	if _, err := second.service.PutClientModelPrice(t.Context(), ClientModelPriceRequest{ClientModel: rows[1].ModelID, ModelPriceUpdateRequest: request}); err != nil {
		t.Fatal(err)
	}
	request = mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"9"`, "false"))
	if _, err := second.service.UpdateModelPrice(t.Context(), rows[0].ID, request); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not publish")
	}
	input := int64(9_000_000_000)
	assertFollowedPrice(t, first, rows[1], &input)
	if quote := quoteClientModel(first, rows[1].ModelID, pricing.ModeStandard); quote.EstimatedCostNanoUSD != 6_000_000_000 {
		t.Fatalf("writer published obsolete client rules: %+v", quote)
	}
}
