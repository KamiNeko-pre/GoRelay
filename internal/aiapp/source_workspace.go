package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
)

const sourceWorkspaceStatus = "source_workspace"

func sourceWorkspace(r agentdata.Run) bool { return r.Status == sourceWorkspaceStatus }

// OpenSourceWorkspace stores host-driven work without scheduling an Eino run.
// Reopening the same goal and scope restores its ledger, not another identity.
func (s *Service) OpenSourceWorkspace(ctx context.Context, goal string, domains []string) (agentdata.Run, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return agentdata.Run{}, fmt.Errorf("goal is required when opening a source workspace")
	}
	unique := map[string]bool{}
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		if domain == "" || strings.ContainsAny(domain, "/:@* \\?#") {
			return agentdata.Run{}, fmt.Errorf("source_scope must contain plain domain names")
		}
		unique[domain] = true
	}
	domains = nil
	for domain := range unique {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	input := CreateRunRequest{Goal: goal, SourceScope: domains, ActionMode: "code"}
	input.RequestKey = "mcp-source:" + agentdata.Hash([]any{goal, domains})
	raw, err := json.Marshal(input)
	if err != nil {
		return agentdata.Run{}, err
	}
	limits := programLimitsForRun(agentdata.Run{})
	budget, err := json.Marshal(BudgetSnapshot{LoopVersion: 5, ProgramLimits: &limits, Candidates: map[string]int{}})
	if err != nil {
		return agentdata.Run{}, err
	}
	var workspace agentdata.Run
	err = s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e error
		workspace, e = agentdata.NewStore(tx).CreateRun(ctx, input.RequestKey, goal, string(raw), string(budget), time.Time{})
		if e != nil {
			return e
		}
		if workspace.Status == "planning" {
			if e := tx.Model(&agentdata.Run{}).Where("id = ? AND status = ?", workspace.ID, "planning").Updates(map[string]any{"status": sourceWorkspaceStatus, "next_wake_at": nil}).Error; e != nil {
				return e
			}
			workspace.Status = sourceWorkspaceStatus
		}
		if !sourceWorkspace(workspace) {
			return fmt.Errorf("source workspace %s is %s; read its saved work instead of resubmitting", workspace.ID, workspace.Status)
		}
		return nil
	})
	return workspace, err
}

func (s *Service) SourceWorkspace(ctx context.Context, id string) (agentdata.Run, error) {
	workspace, err := s.Data.Run(ctx, id)
	if err != nil {
		return workspace, err
	}
	if !sourceWorkspace(workspace) {
		return workspace, fmt.Errorf("workspace_id %q is not an active external source workspace", id)
	}
	return workspace, nil
}

// ExecuteSourceTool is deprecated in the single-tier architecture.
func (s *Service) ExecuteSourceTool(ctx context.Context, id, name string, args any) (out ToolResult, cause error) {
	return ToolResult{}, fmt.Errorf("direct tool execution is deprecated in single-tier architecture")
}

// SourceProgramGuidance shares the execution contract with the internal caller.
func SourceProgramGuidance() string {
	return "Lua source should fetch observations, parse fields according to the schema, and produce records."
}
