// Package identity makes the gateway a Casdoor OpenID Connect relying party:
// it logs users in, keeps their sessions in Redis, and compiles each user's
// teams and roles from Casdoor into a Principal.
package identity

import (
	"slices"
	"strings"
)

// Casdoor permission objects the gateway judges against. The model is
// RBAC with tenants: each team is a domain.
const (
	ModelName      = "gateway_rbac_tenant"
	Resource       = "gateway"
	ActionPlatform = "platform:manage"
	ActionTeam     = "team:manage"
	// PlatformDomain is the literal domain of the platform permission, not a wildcard.
	PlatformDomain = "*"
)

// Principal is the signed-in Casdoor user and what the gateway lets them do.
type Principal struct {
	UserID      string   `json:"user_id"` // Casdoor sub (user.id)
	Name        string   `json:"name"`    // Casdoor user.name
	DisplayName string   `json:"display_name"`
	Email       string   `json:"email"`
	Teams       []string `json:"teams"` // group names without the organization prefix
	Platform    bool     `json:"platform"`
	AdminTeams  []string `json:"admin_teams"` // teams whose team:manage judged true
}

// HasAccess reports whether the user may use the console at all.
func (p Principal) HasAccess() bool {
	return p.Platform || len(p.Teams) > 0
}

// IsAdminOf reports whether the user manages team; platform admins manage every team.
func (p Principal) IsAdminOf(team string) bool {
	return p.Platform || slices.Contains(p.AdminTeams, team)
}

// User is the part of a Casdoor user the gateway reads.
type User struct {
	Owner       string   `json:"owner"`
	Name        string   `json:"name"`
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	Groups      []string `json:"groups"`
	IsForbidden bool     `json:"isForbidden"`
}

// Team is one Casdoor group of the organization.
type Team struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Member is one direct member of a team.
type Member struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// userTeams strips the organization prefix from the user's direct groups.
func userTeams(user User, organization string) []string {
	prefix := organization + "/"
	teams := make([]string, 0, len(user.Groups))
	for _, group := range user.Groups {
		if team, ok := strings.CutPrefix(group, prefix); ok && team != "" && !slices.Contains(teams, team) {
			teams = append(teams, team)
		}
	}
	return teams
}

// enforceRequests asks for platform:manage first, then team:manage on each team in order.
func enforceRequests(user User, teams []string) [][]string {
	subject := user.Owner + "/" + user.Name
	requests := make([][]string, 0, len(teams)+1)
	requests = append(requests, []string{subject, PlatformDomain, Resource, ActionPlatform})
	for _, team := range teams {
		requests = append(requests, []string{subject, team, Resource, ActionTeam})
	}
	return requests
}

// compilePrincipal turns a Casdoor user and the batch-enforce answer to
// enforceRequests into a Principal. Casdoor returns one row per model/adapter
// group; a request is allowed when any row allows it, and missing cells are false.
func compilePrincipal(user User, organization string, decisions [][]bool) Principal {
	teams := userTeams(user, organization)
	allowed := func(column int) bool {
		for _, row := range decisions {
			if column < len(row) && row[column] {
				return true
			}
		}
		return false
	}
	adminTeams := make([]string, 0)
	for index, team := range teams {
		if allowed(index + 1) {
			adminTeams = append(adminTeams, team)
		}
	}
	return Principal{
		UserID:      user.ID,
		Name:        user.Name,
		DisplayName: user.DisplayName,
		Email:       user.Email,
		Teams:       teams,
		Platform:    allowed(0),
		AdminTeams:  adminTeams,
	}
}
