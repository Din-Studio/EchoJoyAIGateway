package control

import (
	"sort"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

func decisionRouteOptions(snapshot *state.ConfigSnapshot) []DecisionRouteOption {
	byGroup := map[uint]map[string]bool{}
	for model, targets := range snapshot.ExecutionCandidates[protocol.Decisions][execution.OperationDecisionsCreate] {
		for _, target := range targets {
			if byGroup[target.GroupID] == nil {
				byGroup[target.GroupID] = map[string]bool{}
			}
			byGroup[target.GroupID][model] = true
		}
	}
	options := []DecisionRouteOption{}
	for id, models := range byGroup {
		names := []string{}
		for name := range models {
			names = append(names, name)
		}
		sort.Strings(names)
		options = append(options, DecisionRouteOption{GroupID: id, GroupName: snapshot.Groups[id].Name, Models: names})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].GroupName < options[j].GroupName })
	return options
}

func auditAccessKeyOptions(snapshot *state.ConfigSnapshot) []AuditAccessKeyOption {
	options := []AuditAccessKeyOption{}
	for _, key := range snapshot.AccessKeysByID {
		options = append(options, AuditAccessKeyOption{ID: key.ID, Name: key.Name})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Name < options[j].Name })
	return options
}
