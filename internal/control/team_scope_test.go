package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/identity"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// Extra sessions for the scope tests: a team-a member without keys, and a
// team-b member whose only key is disabled because team-b has no grants.
const (
	testKeylessMemberSession = "test-keyless-member-session"
	testTeamBMemberSession   = "test-team-b-member-session"
)

type teamScopeFixture struct {
	serviceFixture
	server *Server
	engine *gin.Engine
	// granted is granted to team-a; private is not.
	granted, private uint
	// Keys: a platform key, two team-a keys owned by m1 and alice, a team-b
	// key owned by uid-b, and a team-b key owned by alice.
	platformKey, memberKey, adminKey, teamBKey, aliceTeamBKey uint
}

func newTeamScopeFixture(t *testing.T) teamScopeFixture {
	t.Helper()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	group := func(name string) uint {
		return createPriceTestGroup(t, fixture.db, models.Group{
			Name: name, Models: models.JSON(`[{"id":"` + name + `-model"}]`),
			Overrides: models.JSON(`{}`), Enabled: true,
		}).ID
	}
	scope := teamScopeFixture{serviceFixture: fixture, granted: group("granted"), private: group("private")}
	mustEnsureInitialPrices(t, fixture)
	key := func(name string) uint {
		created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: name})
		if err != nil {
			t.Fatalf("CreateAccessKey(%s) error = %v", name, err)
		}
		return created.ID
	}
	scope.platformKey, scope.memberKey, scope.adminKey, scope.teamBKey, scope.aliceTeamBKey =
		key("platform key"), key("m1 key"), key("alice key"), key("team-b key"), key("alice team-b key")
	_, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		if err := tx.Create(&models.TeamGroupGrant{TeamID: "team-a", GroupID: scope.granted}).Error; err != nil {
			return err
		}
		for id, owner := range map[uint][2]string{
			scope.memberKey: {"team-a", "uid-m1"}, scope.adminKey: {"team-a", "uid-alice"},
			scope.teamBKey: {"team-b", "uid-b"}, scope.aliceTeamBKey: {"team-b", "uid-alice"},
		} {
			if err := tx.Model(&models.AccessKey{}).Where("id = ?", id).Updates(map[string]any{
				"team_id": owner[0], "owner_user_id": owner[1], "owner_name": owner[1],
			}).Error; err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("assign team keys: %v", err)
	}
	scope.server = newTestServer(t, &config.Config{}, fixture.service)
	stub := scope.server.identity.(*stubIdentity)
	stub.sessions[testKeylessMemberSession] = identity.Principal{UserID: "uid-keyless", Name: "keyless", Teams: []string{"team-a"}}
	stub.sessions[testTeamBMemberSession] = identity.Principal{UserID: "uid-b", Name: "b", Teams: []string{"team-b"}}
	stub.members["team-b"] = append(stub.members["team-b"], identity.Member{ID: "uid-b", Name: "b"})
	scope.engine = gin.New()
	scope.engine.Use(gin.Recovery())
	scope.server.RegisterRoutes(scope.engine)
	return scope
}

func (f teamScopeFixture) serve(method, target, cookie string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, target, reader)
	setTestSession(request, cookie)
	request.Header.Set("Accept-Language", "en-US")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost {
		request.Header.Set("Idempotency-Key", fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000))
	}
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	return recorder
}

func decodeData[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body, err)
	}
	return envelope.Data
}

func TestTeamScopeLimitsWhichKeysEachRoleSees(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	var allKeys []uint
	if err := f.db.Model(&models.AccessKey{}).Order("id ASC").Pluck("id", &allKeys).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cookie string
		want   []uint
	}{
		{name: "platform", cookie: testPlatformSession, want: allKeys},
		{name: "team-a admin", cookie: testTeamAdminSession, want: []uint{f.memberKey, f.adminKey, f.aliceTeamBKey}},
		{name: "member", cookie: testMemberSession, want: []uint{f.memberKey}},
		{name: "member without keys", cookie: testKeylessMemberSession, want: []uint{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			list := f.serve(http.MethodGet, "/api/access-keys", test.cookie, nil)
			options := f.serve(http.MethodGet, "/api/access-keys/options", test.cookie, nil)
			if list.Code != http.StatusOK || options.Code != http.StatusOK {
				t.Fatalf("list = %d %s, options = %d %s", list.Code, list.Body, options.Code, options.Body)
			}
			listed := []uint{}
			for _, item := range decodeData[AccessKeyCollectionResponse](t, list).Items {
				listed = append(listed, item.ID)
			}
			optionIDs := []uint{}
			for _, option := range decodeData[[]AccessKeyOption](t, options) {
				optionIDs = append(optionIDs, option.ID)
			}
			slices.Sort(listed)
			if !slices.Equal(listed, test.want) || !slices.Equal(optionIDs, test.want) {
				t.Fatalf("list = %v, options = %v, want %v", listed, optionIDs, test.want)
			}
		})
	}
}

func TestMembersRevealOnlyTheirOwnKeys(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	if own := f.serve(http.MethodPost, fmt.Sprintf("/api/access-keys/%d/reveal", f.memberKey), testMemberSession, nil); own.Code != http.StatusOK {
		t.Fatalf("reveal own key = %d %s", own.Code, own.Body)
	}
	for _, id := range []uint{f.adminKey, f.platformKey, f.teamBKey} {
		other := f.serve(http.MethodPost, fmt.Sprintf("/api/access-keys/%d/reveal", id), testMemberSession, nil)
		if other.Code != http.StatusNotFound {
			t.Fatalf("reveal key %d = %d %s, want 404", id, other.Code, other.Body)
		}
	}
	if team := f.serve(http.MethodPost, fmt.Sprintf("/api/access-keys/%d/reveal", f.memberKey), testTeamAdminSession, nil); team.Code != http.StatusOK {
		t.Fatalf("team admin reveals member key = %d %s", team.Code, team.Body)
	}
}

func TestTeamAdminWritesOnlyTheirTeamsKeys(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	rename := map[string]any{"name": "renamed"}
	for _, test := range []struct {
		cookie string
		id     uint
		want   int
	}{
		{testTeamAdminSession, f.memberKey, http.StatusOK},
		{testTeamAdminSession, f.teamBKey, http.StatusNotFound},
		{testTeamAdminSession, f.platformKey, http.StatusNotFound},
		{testMemberSession, f.memberKey, http.StatusForbidden},
		// alice administers team-a only; her own key in team-b is visible but read-only.
		{testTeamAdminSession, f.aliceTeamBKey, http.StatusForbidden},
		{testPlatformSession, f.teamBKey, http.StatusOK},
	} {
		recorder := f.serve(http.MethodPut, fmt.Sprintf("/api/access-keys/%d", test.id), test.cookie, rename)
		if recorder.Code != test.want {
			t.Fatalf("%s updates key %d = %d %s, want %d", test.cookie, test.id, recorder.Code, recorder.Body, test.want)
		}
	}
	for _, change := range []map[string]any{{"team_id": "team-b"}, {"owner_user_id": "uid-alice"}} {
		recorder := f.serve(http.MethodPut, fmt.Sprintf("/api/access-keys/%d", f.memberKey), testTeamAdminSession, map[string]any{
			"name": "moved", "team_id": change["team_id"], "owner_user_id": change["owner_user_id"],
		})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("changing ownership %v = %d %s, want 400", change, recorder.Code, recorder.Body)
		}
	}
	if same := f.serve(http.MethodPut, fmt.Sprintf("/api/access-keys/%d", f.memberKey), testTeamAdminSession, map[string]any{
		"name": "kept", "team_id": "team-a", "owner_user_id": "uid-m1",
	}); same.Code != http.StatusBadRequest {
		t.Fatalf("resending ownership = %d %s, want 400", same.Code, same.Body)
	}
	if multiplier := f.serve(http.MethodPut, fmt.Sprintf("/api/access-keys/%d", f.memberKey), testTeamAdminSession, map[string]any{
		"price_multiplier": "0",
	}); multiplier.Code != http.StatusForbidden {
		t.Fatalf("team admin sets multiplier = %d %s, want 403", multiplier.Code, multiplier.Body)
	}
	if deleted := f.serve(http.MethodDelete, fmt.Sprintf("/api/access-keys/%d", f.adminKey), testTeamAdminSession, nil); deleted.Code != http.StatusOK {
		t.Fatalf("team admin deletes team key = %d %s", deleted.Code, deleted.Body)
	}
}

func TestTeamKeyCreationFollowsGrantsAndMembership(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	create := func(cookie string, body map[string]any) *httptest.ResponseRecorder {
		return f.serve(http.MethodPost, "/api/access-keys", cookie, body)
	}
	created := create(testTeamAdminSession, map[string]any{
		"name": "for m1", "team_id": "team-a", "owner_user_id": "uid-m1",
		"filters": map[string]any{"groups": []uint{f.granted}}, "rpm_limit": 30,
		"cost_limit_rules": []map[string]any{{"kind": "periodic", "limit_usd": "5", "period_seconds": 86400}},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create team key = %d %s", created.Code, created.Body)
	}
	metadata := decodeData[AccessKeyCreateResult](t, created)
	if metadata.TeamID == nil || *metadata.TeamID != "team-a" || metadata.OwnerUserID == nil || *metadata.OwnerUserID != "uid-m1" ||
		metadata.OwnerName == nil || *metadata.OwnerName != "m1" || metadata.RPMLimit != 30 || len(metadata.CostLimitRules) != 1 {
		t.Fatalf("created metadata = %+v", metadata)
	}
	if view := f.manager.Current().AccessKeysByID[metadata.ID]; view.Status != state.AccessKeyStatusActive {
		t.Fatalf("created team key status = %s", view.Status)
	}
	for _, test := range []struct {
		name   string
		cookie string
		body   map[string]any
		want   int
	}{
		{"group not granted", testTeamAdminSession, map[string]any{"name": "x", "team_id": "team-a", "owner_user_id": "uid-m1", "filters": map[string]any{"groups": []uint{f.private}}}, http.StatusBadRequest},
		{"owner outside team", testTeamAdminSession, map[string]any{"name": "x", "team_id": "team-a", "owner_user_id": "uid-b"}, http.StatusBadRequest},
		{"team without grants", testPlatformSession, map[string]any{"name": "x", "team_id": "team-b", "owner_user_id": "uid-b"}, http.StatusBadRequest},
		{"not admin of team", testTeamAdminSession, map[string]any{"name": "x", "team_id": "team-b", "owner_user_id": "uid-alice"}, http.StatusForbidden},
		{"platform key", testTeamAdminSession, map[string]any{"name": "x"}, http.StatusForbidden},
		{"multiplier", testTeamAdminSession, map[string]any{"name": "x", "team_id": "team-a", "owner_user_id": "uid-m1", "price_multiplier": "0"}, http.StatusForbidden},
		{"member", testMemberSession, map[string]any{"name": "x", "team_id": "team-a", "owner_user_id": "uid-m1"}, http.StatusForbidden},
	} {
		recorder := create(test.cookie, test.body)
		if recorder.Code != test.want {
			t.Fatalf("%s = %d %s, want %d", test.name, recorder.Code, recorder.Body, test.want)
		}
	}
	noGrants := create(testPlatformSession, map[string]any{"name": "x", "team_id": "team-b", "owner_user_id": "uid-b"})
	if !strings.Contains(noGrants.Body.String(), "TEAM_NO_GRANTED_GROUPS") {
		t.Fatalf("team without grants = %s", noGrants.Body)
	}
}

func TestMembersWithoutRoutableKeysGetEmptyReadsNotErrors(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	f.service.requestLogs = requestlog.NewService(f.db, nil, nil)
	f.service.usageStats = requestlog.NewService(f.db, nil, nil)
	f.service.homeStatistics = requestlog.NewService(f.db, nil, nil)
	now := time.Now().UTC()
	for index, keyID := range []uint{f.platformKey, f.memberKey} {
		row := models.RequestLog{
			ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1), CompletedAtMS: now.Add(-time.Minute).UnixMilli(),
			AccessKeyID: keyID, GroupID: f.granted, ChannelID: "openai", CredentialID: 1,
			Protocol: "openai-completions", ClientModel: "m", UpstreamModel: "m", UpstreamReportedModel: "m", ModelConsistency: "match",
			Status: "success", StatusCode: 200, AttemptCount: 1,
			UsageState: "complete", CostState: "priced", PricingCompleteness: "complete",
		}
		if err := f.db.Create(&row).Error; err != nil {
			t.Fatalf("create request log: %v", err)
		}
	}
	usageQuery := usageTestTimeQuery(now.Add(-time.Hour), now)
	for _, test := range []struct {
		cookie   string
		wantLogs int
	}{
		{testKeylessMemberSession, 0},
		{testTeamBMemberSession, 0},
		{testMemberSession, 1},
		{testPlatformSession, 2},
	} {
		logs := f.serve(http.MethodGet, "/api/logs", test.cookie, nil)
		usage := f.serve(http.MethodGet, "/api/usage?"+usageQuery, test.cookie, nil)
		if logs.Code != http.StatusOK || usage.Code != http.StatusOK {
			t.Fatalf("%s logs = %d %s, usage = %d %s", test.cookie, logs.Code, logs.Body, usage.Code, usage.Body)
		}
		page := decodeData[struct {
			Items []json.RawMessage `json:"items"`
		}](t, logs)
		if len(page.Items) != test.wantLogs {
			t.Fatalf("%s sees %d logs, want %d", test.cookie, len(page.Items), test.wantLogs)
		}
		if got := decodeData[usageResponse](t, usage).Summary.RequestCount; got != int64(test.wantLogs) {
			t.Fatalf("%s usage requests = %d, want %d", test.cookie, got, test.wantLogs)
		}
	}
	for _, cookie := range []string{testKeylessMemberSession, testTeamBMemberSession} {
		home := f.serve(http.MethodGet, "/api/home", cookie, nil)
		statistics := f.serve(http.MethodGet, "/api/home/statistics?range=24h", cookie, nil)
		modelsList := f.serve(http.MethodGet, "/api/models", cookie, nil)
		if home.Code != http.StatusOK || statistics.Code != http.StatusOK || modelsList.Code != http.StatusOK {
			t.Fatalf("%s home = %d %s, statistics = %d, models = %d %s", cookie, home.Code, home.Body, statistics.Code, modelsList.Code, modelsList.Body)
		}
		if base := decodeData[HomeBase](t, home); len(base.AccessKeys) != 0 || base.Inventory != (HomeInventory{}) {
			t.Fatalf("%s home = %+v", cookie, base)
		}
		if items := decodeData[ProjectModelListResponse](t, modelsList).Items; len(items) != 0 {
			t.Fatalf("%s models = %+v", cookie, items)
		}
	}
	memberModels := decodeData[ProjectModelListResponse](t, f.serve(http.MethodGet, "/api/models", testMemberSession, nil))
	if len(memberModels.Items) != 1 || memberModels.Items[0].ClientModel != "granted-model" {
		t.Fatalf("member models = %+v", memberModels.Items)
	}
}

func TestTeamGrantsAreManagedByPlatformAdmins(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	list := f.serve(http.MethodGet, "/api/teams", testPlatformSession, nil)
	teams := decodeData[teamListResponse](t, list).Items
	if list.Code != http.StatusOK || len(teams) != 2 || !slices.Equal(teams[0].GroupIDs, []uint{f.granted}) || len(teams[1].GroupIDs) != 0 {
		t.Fatalf("teams = %d %+v", list.Code, teams)
	}
	if member := f.serve(http.MethodGet, "/api/teams", testTeamAdminSession, nil); member.Code != http.StatusForbidden {
		t.Fatalf("team admin lists teams = %d", member.Code)
	}

	detail := f.serve(http.MethodGet, "/api/teams/team-a", testTeamAdminSession, nil)
	team := decodeData[TeamDetail](t, detail)
	if detail.Code != http.StatusOK || len(team.Groups) != 1 || team.Groups[0].ID != f.granted || len(team.Members) != 2 {
		t.Fatalf("team detail = %d %+v", detail.Code, team)
	}
	if other := f.serve(http.MethodGet, "/api/teams/team-b", testTeamAdminSession, nil); other.Code != http.StatusNotFound {
		t.Fatalf("team admin reads another team = %d", other.Code)
	}
	if member := f.serve(http.MethodGet, "/api/teams/team-a", testMemberSession, nil); member.Code != http.StatusForbidden {
		t.Fatalf("member reads team = %d", member.Code)
	}

	if unknown := f.serve(http.MethodPut, "/api/teams/team-z/groups", testPlatformSession, map[string]any{"group_ids": []uint{f.granted}}); unknown.Code != http.StatusNotFound {
		t.Fatalf("grant unknown team = %d %s", unknown.Code, unknown.Body)
	}
	// Revoking every grant disables team-a's keys on the next snapshot.
	revoke := f.serve(http.MethodPut, "/api/teams/team-a/groups", testPlatformSession, map[string]any{"group_ids": []uint{}})
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", revoke.Code, revoke.Body)
	}
	if view := f.manager.Current().AccessKeysByID[f.memberKey]; view.Status != state.AccessKeyStatusDisabled {
		t.Fatalf("member key after revoke = %s", view.Status)
	}
	// The list still says active (the row is) but flags why the gateway rejects the key.
	blocked := map[uint]bool{}
	for _, item := range decodeData[AccessKeyCollectionResponse](t, f.serve(http.MethodGet, "/api/access-keys", testPlatformSession, nil)).Items {
		blocked[item.ID] = item.BlockedByTeamGrants
		if item.ID == f.memberKey && item.Status != state.AccessKeyStatusActive {
			t.Fatalf("member key row status after revoke = %s", item.Status)
		}
	}
	if !blocked[f.memberKey] || !blocked[f.teamBKey] || blocked[f.platformKey] {
		t.Fatalf("blocked_by_team_grants = %v", blocked)
	}
	grant := f.serve(http.MethodPut, "/api/teams/team-b/groups", testPlatformSession, map[string]any{"group_ids": []uint{f.private, f.private}})
	if grant.Code != http.StatusOK || !slices.Equal(decodeData[TeamItem](t, grant).GroupIDs, []uint{f.private}) {
		t.Fatalf("grant = %d %s", grant.Code, grant.Body)
	}
	view := f.manager.Current().AccessKeysByID[f.teamBKey]
	if _, routes := view.Filters.Groups[f.private]; view.Status != state.AccessKeyStatusActive || !routes {
		t.Fatalf("team-b key after grant = %+v", view)
	}
}

// TestEveryControlRouteDeclaresItsAccess checks the access table of section
// 4.3 through behavior: members reach only session routes, team admins also
// reach team-admin routes, and neither reaches platform routes.
func TestEveryControlRouteDeclaresItsAccess(t *testing.T) {
	t.Parallel()
	f := newTeamScopeFixture(t)
	session := []string{
		"control.auth.session", "control.models.list", "control.home", "control.home.statistics",
		"control.logs.list", "control.logs.get", "control.usage",
		"control.access-keys.list", "control.access-keys.options", "control.access-keys.reveal",
	}
	teamAdmin := []string{
		"control.access-keys.create", "control.access-keys.update", "control.access-keys.rotate",
		"control.access-keys.delete", "control.access-keys.cost-limits.reset", "control.teams.get",
	}
	for _, route := range f.server.HTTPModule().Routes {
		target := "/api" + route.Path
		for _, param := range []string{":group_id", ":credential_id", ":id", ":stage_id"} {
			target = strings.ReplaceAll(target, param, "999")
		}
		target = strings.ReplaceAll(target, ":request_id", "00000000-0000-4000-8000-000000000999")
		target = strings.ReplaceAll(target, ":team", "team-a")
		for _, actor := range []struct {
			cookie  string
			reaches []string
		}{
			{testMemberSession, session},
			{testTeamAdminSession, append(slices.Clone(session), teamAdmin...)},
		} {
			recorder := f.serve(route.Methods[0], target, actor.cookie, nil)
			forbidden := recorder.Code == http.StatusForbidden && strings.Contains(recorder.Body.String(), `"FORBIDDEN"`)
			if forbidden == slices.Contains(actor.reaches, route.Name) {
				t.Errorf("%s %s as %s = %d %s", route.Name, target, actor.cookie, recorder.Code, recorder.Body)
			}
		}
	}
}
