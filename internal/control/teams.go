package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/identity"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/storage/models"
)

const maxTeamNameRunes = 255

// TeamItem is one Casdoor team and the groups granted to it.
type TeamItem struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	GroupIDs    []uint `json:"group_ids"`
}

type teamListResponse struct {
	Items []TeamItem `json:"items"`
}

type teamGroupsRequest struct {
	GroupIDs []uint `json:"group_ids"`
}

type TeamGroup struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// TeamDetail is what a team admin needs to create keys for the team.
type TeamDetail struct {
	Groups  []TeamGroup       `json:"groups"`
	Members []identity.Member `json:"members"`
}

// ListTeamGrants returns the granted groups of every team that has any.
func (s *Service) ListTeamGrants(ctx context.Context) (map[string][]uint, error) {
	var rows []models.TeamGroupGrant
	if err := s.db.WithContext(ctx).Order("team_id ASC, group_id ASC").Find(&rows).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	grants := make(map[string][]uint)
	for _, row := range rows {
		grants[row.TeamID] = append(grants[row.TeamID], row.GroupID)
	}
	return grants, nil
}

// ReplaceTeamGrants sets the groups granted to team. The snapshot reload that
// follows narrows or disables the team's keys on every replica.
func (s *Service) ReplaceTeamGrants(ctx context.Context, team string, groupIDs []uint) ([]uint, error) {
	granted := make([]uint, 0, len(groupIDs))
	for _, id := range groupIDs {
		if id == 0 {
			return nil, app_errors.ErrValidation
		}
		if !slices.Contains(granted, id) {
			granted = append(granted, id)
		}
	}
	slices.Sort(granted)
	_, err := s.writeConfig(ctx, func(tx *gorm.DB) error {
		if err := validateFilterGroupReferences(tx, granted); err != nil {
			return err
		}
		if err := tx.Where("team_id = ?", team).Delete(&models.TeamGroupGrant{}).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		if len(granted) == 0 {
			return nil
		}
		rows := make([]models.TeamGroupGrant, 0, len(granted))
		for _, id := range granted {
			rows = append(rows, models.TeamGroupGrant{TeamID: team, GroupID: id})
		}
		if err := tx.Create(&rows).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return granted, nil
}

// ListTeamGroups returns the groups granted to team with their names.
func (s *Service) ListTeamGroups(ctx context.Context, team string) ([]TeamGroup, error) {
	groups := []TeamGroup{}
	if err := s.db.WithContext(ctx).Model(&models.Group{}).
		Select("groups.id", "groups.name").
		Joins("JOIN team_group_grants ON team_group_grants.group_id = groups.id").
		Where("team_group_grants.team_id = ?", team).
		Order("groups.id ASC").
		Scan(&groups).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return groups, nil
}

func (s *Server) handleListTeams(c *gin.Context) {
	teams, err := s.identity.Teams(c.Request.Context())
	if err != nil {
		writeServiceError(c, "list_teams", fmt.Errorf("read Casdoor teams: %w: %w", app_errors.ErrBadGateway, err))
		return
	}
	grants, err := s.service.ListTeamGrants(c.Request.Context())
	if err != nil {
		writeServiceError(c, "list_teams", err)
		return
	}
	items := make([]TeamItem, 0, len(teams))
	for _, team := range teams {
		groupIDs := grants[team.Name]
		if groupIDs == nil {
			groupIDs = []uint{}
		}
		items = append(items, TeamItem{Name: team.Name, DisplayName: team.DisplayName, GroupIDs: groupIDs})
	}
	response.SuccessI18n(c, "common.success", teamListResponse{Items: items})
}

func (s *Server) handleUpdateTeamGroups(c *gin.Context) {
	team, ok := teamParam(c, "update_team_groups")
	if !ok {
		return
	}
	var request teamGroupsRequest
	if err := bindStrictJSON(c, &request); err != nil {
		writeServiceError(c, "update_team_groups", mapControlJSONError(err))
		return
	}
	if request.GroupIDs == nil {
		writeServiceError(c, "update_team_groups", app_errors.ErrValidation)
		return
	}
	teams, err := s.identity.Teams(c.Request.Context())
	if err != nil {
		writeServiceError(c, "update_team_groups", fmt.Errorf("read Casdoor teams: %w: %w", app_errors.ErrBadGateway, err))
		return
	}
	if !slices.ContainsFunc(teams, func(item identity.Team) bool { return item.Name == team }) {
		writeServiceError(c, "update_team_groups", app_errors.ErrResourceNotFound)
		return
	}
	granted, err := s.service.ReplaceTeamGrants(c.Request.Context(), team, request.GroupIDs)
	if err != nil {
		writeServiceError(c, "update_team_groups", err)
		return
	}
	response.SuccessI18n(c, "common.success", TeamItem{Name: team, GroupIDs: granted})
}

func (s *Server) handleGetTeam(c *gin.Context) {
	team, ok := teamParam(c, "get_team")
	if !ok {
		return
	}
	if principal, _ := currentPrincipal(c); !principal.IsAdminOf(team) {
		writeServiceError(c, "get_team", app_errors.ErrResourceNotFound)
		return
	}
	groups, err := s.service.ListTeamGroups(c.Request.Context(), team)
	if err != nil {
		writeServiceError(c, "get_team", err)
		return
	}
	members, err := s.identity.TeamMembers(c.Request.Context(), team)
	if err != nil {
		writeServiceError(c, "get_team", fmt.Errorf("read Casdoor team members: %w: %w", app_errors.ErrBadGateway, err))
		return
	}
	if members == nil {
		members = []identity.Member{}
	}
	response.SuccessI18n(c, "common.success", TeamDetail{Groups: groups, Members: members})
}

func teamParam(c *gin.Context, operation string) (string, bool) {
	team := c.Param("team")
	if strings.TrimSpace(team) != team || team == "" || team == identity.PlatformDomain ||
		utf8.RuneCountInString(team) > maxTeamNameRunes {
		writeServiceError(c, operation, app_errors.ErrBadRequest)
		return "", false
	}
	return team, true
}

func teamMutationLocator(c *gin.Context) string {
	team := c.Param("team")
	if utf8.RuneCountInString(team) > maxTeamNameRunes {
		return "team:unknown"
	}
	return "team:" + team
}
