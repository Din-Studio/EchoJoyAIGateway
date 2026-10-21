package state

import (
	"slices"
	"strings"
)

// GroupModelKey 将轮询进度限定在同组同名模型，组内凭据共享进度。
type GroupModelKey struct {
	GroupID       uint
	ExternalModel string
}

// SelectModel 接收按上游 ID 排序的可用模型；只消费模型轮次，不修改凭据份额。
func (s *SchedulingState) SelectModel(groupID uint, externalModel string, models []string) string {
	if len(models) == 0 {
		return ""
	}
	selected := models[0]
	s.WithLock(func(ledger *SchedulingLedger) {
		key := GroupModelKey{GroupID: groupID, ExternalModel: externalModel}
		order := ledger.ModelCursors[key]
		for index, model := range order {
			if _, available := slices.BinarySearch(models, model); !available {
				continue
			}
			selected = model
			// 只把本次选中的模型移到队尾，保留其他凭据仍可用模型的先后顺序。
			copy(order[index:], order[index+1:])
			order[len(order)-1] = model
			break
		}
	})
	return selected
}

func (s *SchedulingState) syncModelCursorsLocked(snapshot *ConfigSnapshot) {
	cursors := make(map[GroupModelKey][]string)
	for key, order := range s.ledger.ModelCursors {
		if group, exists := snapshot.GroupCatalog[key.GroupID]; exists && !group.Enabled {
			cursors[key] = order
		}
	}
	for groupID, group := range snapshot.Groups {
		modelsByName := make(map[string][]string)
		for _, model := range group.Models {
			name := externalModelName(model)
			modelsByName[name] = append(modelsByName[name], strings.TrimSpace(model.ID))
		}
		for name, models := range modelsByName {
			if len(models) > 1 {
				slices.Sort(models)
				key := GroupModelKey{GroupID: groupID, ExternalModel: name}
				previous, known := s.ledger.ModelCursors[key]
				order := reconcileModelOrder(previous, models)
				if !known {
					order = s.ledger.staggerModelOrder(order)
				}
				cursors[key] = order
			}
		}
	}
	s.ledger.ModelCursors = cursors
}

// staggerModelOrder 让新建的轮询队列从随机位置开始，理由同 Stagger：
// 游标按副本独立，同起点会让各副本先选中同一个上游模型。
func (d *SchedulingLedger) staggerModelOrder(order []string) []string {
	if d.jitter == nil || len(order) < 2 {
		return order
	}
	offset := int(d.jitter(uint64(len(order))))
	return slices.Concat(order[offset:], order[:offset])
}

// 配置发布保留现存模型的轮询顺序，新模型追加到队尾。
func reconcileModelOrder(previous, configured []string) []string {
	remaining := make(map[string]struct{}, len(configured))
	for _, model := range configured {
		remaining[model] = struct{}{}
	}
	order := make([]string, 0, len(configured))
	for _, model := range previous {
		if _, exists := remaining[model]; exists {
			order = append(order, model)
			delete(remaining, model)
		}
	}
	for _, model := range configured {
		if _, exists := remaining[model]; exists {
			order = append(order, model)
			delete(remaining, model)
		}
	}
	return order
}
