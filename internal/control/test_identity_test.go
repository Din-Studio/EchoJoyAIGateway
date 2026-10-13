package control

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpt-load/internal/identity"
	"gpt-load/internal/storage/models"
)

const testPublicURL = "http://gateway.test"

// Session cookies the stub identity knows.
const (
	testPlatformSession  = "test-platform-session"
	testTeamAdminSession = "test-team-admin-session"
	testMemberSession    = "test-member-session"
	// authTestKey is the platform session older tests pass around by value.
	authTestKey = "test-auth-key"
)

// Principals behind the test sessions: alice manages team-a and is a plain
// member of team-b; m1 is a member of team-a.
var (
	testPlatformPrincipal = identity.Principal{
		UserID: "uid-platform", Name: "platform", DisplayName: "Platform", Platform: true,
		Teams: []string{}, AdminTeams: []string{},
	}
	testTeamAdminPrincipal = identity.Principal{
		UserID: "uid-alice", Name: "alice", DisplayName: "Alice",
		Teams: []string{"team-a", "team-b"}, AdminTeams: []string{"team-a"},
	}
	testMemberPrincipal = identity.Principal{
		UserID: "uid-m1", Name: "m1", DisplayName: "M1",
		Teams: []string{"team-a"}, AdminTeams: []string{},
	}
)

// legacyPlatformSessions are the per-test admin credentials older tests still
// pass around; each signs in as the platform administrator.
var legacyPlatformSessions = []string{
	"batch-test-auth", "channel-auth", "credential-auth", "credential-download-all-auth",
	"credential-download-auth", "credential-probe-route-auth", "disabled-group-auth",
	"multipart-test-auth", "protocol-auth", "restore-log-auth", "synthetic-export-auth",
	"credential-duplicate-inspection-auth", "credential-http-auth", "credential-probe-auth",
	"credential-probe-failure-auth", "distinctive-auth-secret", "distinctive-control-auth-key",
	"gl-control-auth-secret-0001", "settings-auth-key", "settings-template-test-auth",
	"subscription-probe-auth",
}

// stubIdentity maps session cookies to Principals and serves fixed Casdoor teams.
type stubIdentity struct {
	mu       sync.Mutex
	sessions map[string]identity.Principal
	teams    []identity.Team
	members  map[string][]identity.Member
	err      error
}

func newStubIdentity() *stubIdentity {
	stub := &stubIdentity{
		sessions: map[string]identity.Principal{
			testPlatformSession:  testPlatformPrincipal,
			authTestKey:          testPlatformPrincipal,
			testTeamAdminSession: testTeamAdminPrincipal,
			testMemberSession:    testMemberPrincipal,
		},
		teams: []identity.Team{{Name: "team-a", DisplayName: "Team A"}, {Name: "team-b", DisplayName: "Team B"}},
		members: map[string][]identity.Member{
			"team-a": {{ID: "uid-alice", Name: "alice", DisplayName: "Alice"}, {ID: "uid-m1", Name: "m1", DisplayName: "M1"}},
			"team-b": {{ID: "uid-alice", Name: "alice", DisplayName: "Alice"}},
		},
	}
	for _, session := range legacyPlatformSessions {
		stub.sessions[session] = testPlatformPrincipal
	}
	return stub
}

func (s *stubIdentity) Authenticate(_ context.Context, cookie string) (identity.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return identity.Principal{}, s.err
	}
	principal, ok := s.sessions[cookie]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return principal, nil
}

func (s *stubIdentity) Teams(context.Context) ([]identity.Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]identity.Team(nil), s.teams...), s.err
}

func (s *stubIdentity) TeamMembers(_ context.Context, team string) ([]identity.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]identity.Member(nil), s.members[team]...), s.err
}

// setTestSession sends cookie as the console session from the public origin.
func setTestSession(request *http.Request, cookie string) {
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: cookie})
	}
	request.Header.Set("Origin", testPublicURL)
}

// setTestPlatformSession signs request in as a platform administrator.
func setTestPlatformSession(request *http.Request) {
	setTestSession(request, testPlatformSession)
}

// assignTestTeamKey turns key into a team key owned by ownerID and grants
// every existing group to team, so the key stays routable.
func assignTestTeamKey(t testing.TB, fixture serviceFixture, keyID uint, team, ownerID string) {
	t.Helper()
	_, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		var groupIDs []uint
		if err := tx.Model(&models.Group{}).Pluck("id", &groupIDs).Error; err != nil {
			return err
		}
		for _, groupID := range groupIDs {
			grant := models.TeamGroupGrant{TeamID: team, GroupID: groupID}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&grant).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.AccessKey{}).Where("id = ?", keyID).Updates(map[string]any{
			"team_id": team, "owner_user_id": ownerID, "owner_name": ownerID,
		}).Error
	}, nil)
	if err != nil {
		t.Fatalf("assign team key: %v", err)
	}
}
