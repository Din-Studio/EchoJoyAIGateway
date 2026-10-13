package identity

import (
	"reflect"
	"testing"
)

func TestCompilePrincipalJudgesEachTeamSeparately(t *testing.T) {
	user := User{
		Owner: "echojoy", Name: "alice", ID: "uid-alice",
		Groups: []string{"echojoy/team-a", "echojoy/team-b", "other/team-c", "echojoy/team-a"},
	}
	teams := userTeams(user, "echojoy")
	if !reflect.DeepEqual(teams, []string{"team-a", "team-b"}) {
		t.Fatalf("teams = %v", teams)
	}
	requests := enforceRequests(user, teams)
	want := [][]string{
		{"echojoy/alice", "*", "gateway", "platform:manage"},
		{"echojoy/alice", "team-a", "gateway", "team:manage"},
		{"echojoy/alice", "team-b", "gateway", "team:manage"},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v", requests)
	}

	tests := []struct {
		name         string
		decisions    [][]bool
		wantPlatform bool
		wantAdmin    []string
	}{
		{name: "admin of team-a only", decisions: [][]bool{{false, true, false}}, wantAdmin: []string{"team-a"}},
		{name: "platform", decisions: [][]bool{{true, false, false}}, wantPlatform: true, wantAdmin: []string{}},
		{name: "rows are ORed by column", decisions: [][]bool{{false, false, true}, {false, true, false}}, wantAdmin: []string{"team-a", "team-b"}},
		{name: "empty answer is false", decisions: [][]bool{{}}, wantAdmin: []string{}},
		{name: "no rows", decisions: nil, wantAdmin: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := compilePrincipal(user, "echojoy", test.decisions)
			if got.Platform != test.wantPlatform || !reflect.DeepEqual(got.AdminTeams, test.wantAdmin) {
				t.Fatalf("principal = %+v", got)
			}
			if !reflect.DeepEqual(got.Teams, teams) || got.UserID != "uid-alice" || !got.HasAccess() {
				t.Fatalf("principal = %+v", got)
			}
		})
	}
}

func TestPrincipalAccess(t *testing.T) {
	member := Principal{Teams: []string{"team-a", "team-b"}, AdminTeams: []string{"team-a"}}
	if !member.IsAdminOf("team-a") || member.IsAdminOf("team-b") {
		t.Fatalf("member admin = %+v", member)
	}
	platform := Principal{Platform: true}
	if !platform.HasAccess() || !platform.IsAdminOf("anything") {
		t.Fatalf("platform = %+v", platform)
	}
	if (Principal{}).HasAccess() {
		t.Fatal("principal without teams has access")
	}
}

func TestSafeRedirectStaysOnSite(t *testing.T) {
	for raw, want := range map[string]string{
		"/groups?x=1":          "/groups?x=1",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
	} {
		if got := safeRedirect(raw); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", raw, got, want)
		}
	}
}
