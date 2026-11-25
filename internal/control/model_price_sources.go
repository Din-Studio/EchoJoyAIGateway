package control

import (
	"context"
	"slices"
	"sort"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/catalog"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

type ModelPriceSourceGroupDTO struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type ModelPriceSourceDTO struct {
	Price   ModelPriceDTO              `json:"price"`
	Groups  []ModelPriceSourceGroupDTO `json:"groups"`
	Aliases []string                   `json:"aliases"`
}

type ModelPriceSourcesResponse struct {
	Items []ModelPriceSourceDTO `json:"items"`
}

func (s *Server) handleModelPriceSources(c *gin.Context) {
	if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
		writeServiceError(c, "model_price_sources", app_errors.ErrBadRequest)
		return
	}
	result, err := s.service.ModelPriceSources(c.Request.Context())
	if err != nil {
		writeServiceError(c, "model_price_sources", err)
		return
	}
	response.SuccessI18n(c, "common.success", result)
}

// ModelPriceSources returns grouped prices in the system currency of the read snapshot.
func (s *Service) ModelPriceSources(ctx context.Context) (ModelPriceSourcesResponse, error) {
	result := ModelPriceSourcesResponse{Items: []ModelPriceSourceDTO{}}
	if ctx == nil {
		ctx = context.Background()
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	var snapshot *catalog.Snapshot
	if s.catalogRuntime != nil {
		snapshot = s.catalogRuntime.Load()
	}
	err := s.withReadSnapshot(ctx, func(tx *gorm.DB) error {
		currency, err := loadSettingsCurrency(tx)
		if err != nil {
			return err
		}
		var groups []models.Group
		if err := tx.Order("id ASC").Find(&groups).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		members := make(map[pricing.Identity]*ModelPriceSourceDTO)
		references, err := buildPriceReferenceSnapshot(groups, func(id pricing.Identity, group *models.Group, alias string) {
			item := members[id]
			if item == nil {
				item = &ModelPriceSourceDTO{Aliases: []string{}}
				members[id] = item
			}
			// Each group's entries are contiguous; include the group only once.
			if n := len(item.Groups); n == 0 || item.Groups[n-1].ID != group.ID {
				item.Groups = append(item.Groups, ModelPriceSourceGroupDTO{ID: group.ID, Name: group.Name, Enabled: group.Enabled})
			}
			if alias != "" {
				item.Aliases = append(item.Aliases, alias)
			}
		})
		if err != nil {
			return err
		}
		var rows []models.ModelPrice
		if err := tx.Order("channel_id ASC, model_id ASC").Find(&rows).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		for _, row := range rows {
			item := members[pricing.Identity{ChannelID: row.ChannelID, ModelID: row.ModelID}]
			if item == nil {
				continue
			}
			dto, err := projectModelPriceInCurrency(row, references, snapshot, currency)
			if err != nil {
				return err
			}
			item.Price = dto
			result.Items = append(result.Items, *item)
		}
		return nil
	})
	if err != nil {
		return ModelPriceSourcesResponse{}, err
	}
	for index := range result.Items {
		sort.Strings(result.Items[index].Aliases)
		result.Items[index].Aliases = slices.Compact(result.Items[index].Aliases)
	}
	return result, nil
}
