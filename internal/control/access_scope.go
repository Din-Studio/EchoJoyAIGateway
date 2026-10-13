package control

import (
	"context"
	"fmt"
	"slices"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/identity"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// scopedAccessKeyIDs returns the AccessKeys p may read: nil (every key) for
// platform admins, otherwise the keys of the teams they manage plus their
// own. It must stay one query: an empty result has to remain an empty slice,
// because nil would mean "every key".
func (s *Service) scopedAccessKeyIDs(ctx context.Context, p identity.Principal) ([]uint, error) {
	if p.Platform {
		return nil, nil
	}
	query := s.db.WithContext(ctx).Model(&models.AccessKey{})
	if len(p.AdminTeams) > 0 {
		query = query.Where("team_id IN ? OR owner_user_id = ?", p.AdminTeams, p.UserID)
	} else {
		query = query.Where("owner_user_id = ?", p.UserID)
	}
	ids := []uint{}
	if err := query.Order("id ASC").Pluck("id", &ids).Error; err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, app_errors.ParseDBError(err)
	}
	if ids == nil {
		ids = []uint{}
	}
	return ids, nil
}

// scopeAccessKeyFilter narrows the Principal's scope to an optional single
// requested key. A requested key outside the scope is ErrResourceNotFound.
func (s *Service) scopeAccessKeyFilter(
	ctx context.Context,
	p identity.Principal,
	requested []uint,
) ([]uint, error) {
	ids, err := s.scopedAccessKeyIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		return requested, nil
	}
	if requested == nil {
		return ids, nil
	}
	for _, id := range requested {
		if !slices.Contains(ids, id) {
			return nil, app_errors.ErrResourceNotFound
		}
	}
	return requested, nil
}

type accessKeyOwner struct {
	TeamID      *string
	OwnerUserID *string
}

func (s *Service) loadAccessKeyOwner(ctx context.Context, id uint) (accessKeyOwner, error) {
	var owner accessKeyOwner
	err := s.db.WithContext(ctx).Model(&models.AccessKey{}).
		Select("team_id", "owner_user_id").Where("id = ?", id).Take(&owner).Error
	if err != nil {
		return accessKeyOwner{}, app_errors.ParseDBError(err)
	}
	return owner, nil
}

func (owner accessKeyOwner) readableBy(p identity.Principal) bool {
	return p.Platform ||
		(owner.OwnerUserID != nil && *owner.OwnerUserID == p.UserID) ||
		owner.writableBy(p)
}

func (owner accessKeyOwner) writableBy(p identity.Principal) bool {
	if owner.TeamID == nil {
		return p.Platform
	}
	return p.IsAdminOf(*owner.TeamID)
}

// authorizeAccessKeyRead hides keys outside the Principal's scope as missing.
func (s *Service) authorizeAccessKeyRead(ctx context.Context, id uint, p identity.Principal) error {
	if p.Platform {
		return nil
	}
	owner, err := s.loadAccessKeyOwner(ctx, id)
	if err != nil {
		return err
	}
	if !owner.readableBy(p) {
		return app_errors.ErrResourceNotFound
	}
	return nil
}

// authorizeAccessKeyWrite admits platform admins and admins of the key's team.
// A member's own key is visible but read-only (403); other keys are missing (404).
func (s *Service) authorizeAccessKeyWrite(ctx context.Context, id uint, p identity.Principal) error {
	if p.Platform {
		return nil
	}
	owner, err := s.loadAccessKeyOwner(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case owner.writableBy(p):
		return nil
	case owner.readableBy(p):
		return app_errors.ErrForbidden
	default:
		return app_errors.ErrResourceNotFound
	}
}

// activeScopedKeys returns the routable snapshot views of the scoped keys.
// Disabled keys are skipped: a team key without granted groups compiles to
// nil Filters.Groups, which would read as "every group".
func activeScopedKeys(snapshot *state.ConfigSnapshot, ids []uint) []state.AccessKeyView {
	keys := make([]state.AccessKeyView, 0, len(ids))
	for _, id := range ids {
		key, exists := snapshot.AccessKeysByID[id]
		if exists && key.Status == state.AccessKeyStatusActive {
			keys = append(keys, key)
		}
	}
	return keys
}

// teamKeyBlocked reports whether an active team key was compiled disabled
// because no group of its own is granted to its team.
func teamKeyBlocked(snapshot *state.ConfigSnapshot, key AccessKeyMetadata) bool {
	if key.TeamID == nil || key.Status != state.AccessKeyStatusActive || snapshot == nil {
		return false
	}
	compiled, exists := snapshot.AccessKeysByID[key.ID]
	return exists && compiled.Status != state.AccessKeyStatusActive
}

// loadTeamGrants returns the groups granted to team in ascending order.
func loadTeamGrants(tx *gorm.DB, team string) ([]uint, error) {
	grants := []uint{}
	if err := tx.Model(&models.TeamGroupGrant{}).Where("team_id = ?", team).
		Order("group_id ASC").Pluck("group_id", &grants).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return grants, nil
}

// validateTeamKeyGroups requires granted groups for team and filter groups
// within them; an empty filter means every granted group.
func validateTeamKeyGroups(tx *gorm.DB, team string, groups []uint) error {
	grants, err := loadTeamGrants(tx, team)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return app_errors.ErrTeamNoGrantedGroups
	}
	for _, group := range groups {
		if !slices.Contains(grants, group) {
			return app_errors.ErrValidation
		}
	}
	return nil
}

// requireAccessKeyWrite writes the error response and returns false when the
// signed-in Principal may not change key id.
func (s *Server) requireAccessKeyWrite(c *gin.Context, operation string, id uint) bool {
	principal, _ := currentPrincipal(c)
	if err := s.service.authorizeAccessKeyWrite(c.Request.Context(), id, principal); err != nil {
		writeServiceError(c, operation, err)
		return false
	}
	return true
}

// authorizeAccessKeyCreate admits platform keys from platform admins and team
// keys from admins of that team, and returns the resolved owner: nil for a
// platform key, otherwise a direct member of the team with their Casdoor name.
func (s *Server) authorizeAccessKeyCreate(c *gin.Context, request AccessKeyCreateRequest) (*accessKeyTeamOwner, error) {
	principal, _ := currentPrincipal(c)
	if request.PriceMultiplier.Set && !principal.Platform {
		return nil, app_errors.ErrForbidden
	}
	owner, err := requestedAccessKeyOwner(request)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		if !principal.Platform {
			return nil, app_errors.ErrForbidden
		}
		return nil, nil
	}
	if !principal.IsAdminOf(owner.TeamID) {
		return nil, app_errors.ErrForbidden
	}
	members, err := s.identity.TeamMembers(c.Request.Context(), owner.TeamID)
	if err != nil {
		return nil, fmt.Errorf("read team %q members: %w: %w", owner.TeamID, app_errors.ErrBadGateway, err)
	}
	for _, member := range members {
		if member.ID == owner.OwnerUserID {
			owner.OwnerName = member.Name
			return owner, nil
		}
	}
	return nil, app_errors.ErrValidation
}
