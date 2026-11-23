package information

import "go-lua-crawler/internal/agentstatus"

// TaskRequirements contains user requirements, never a collector's local schema.
type TaskRequirements struct {
	Revision              uint64                            `json:"revision"`
	OriginalText          string                            `json:"original_text"`
	Additions             []string                          `json:"additions,omitempty"`
	SourceScope           []string                          `json:"source_scope,omitempty"`
	ExplicitOutput        []OutputRequirement               `json:"explicit_output,omitempty"`
	InterpretationChanges []RequirementInterpretationChange `json:"interpretation_changes,omitempty"`
}

type RequirementInterpretation struct {
	RequirementsRevision uint64             `json:"requirements_revision"`
	RequirementID        string             `json:"requirement_id"`
	SourceText           string             `json:"source_text"`
	Reason               string             `json:"reason"`
	Output               *OutputRequirement `json:"output,omitempty"`
}

type RequirementInterpretationChange struct {
	ReceiptID  uint64             `json:"receipt_id"`
	Revision   uint64             `json:"revision"`
	SourceText string             `json:"source_text"`
	Reason     string             `json:"reason"`
	Previous   *OutputRequirement `json:"previous,omitempty"`
	Current    *OutputRequirement `json:"current,omitempty"`
}

type OutputRequirement struct {
	ID         string   `json:"id"`
	Origin     string   `json:"origin"`
	SourceText string   `json:"source_text"`
	Kind       string   `json:"kind" jsonschema:"enum=artifact,enum=fields,enum=count"`
	Target     string   `json:"target" jsonschema:"description=报告使用 report；表格使用交付表格的确切标题。fields 与 count 描述指定表格，不代表普通事实问题必须生成表格。"`
	Fields     []string `json:"fields,omitempty"`
	MinCount   *int     `json:"min_count,omitempty"`
	CountBy    []string `json:"count_by,omitempty"`
}

type WorkPlan struct {
	Interpretation    string          `json:"interpretation"`
	Route             string          `json:"route,omitempty" jsonschema:"enum=direct,enum=api,enum=lua,enum=browser,enum=asset,description=单来源计划的默认路线；多来源在 sources 逐项说明。Browser 权限由执行结果决定。"`
	Sources           []PlannedSource `json:"sources,omitempty"`
	NeedSearch        bool            `json:"need_search,omitempty" jsonschema:"description=当前无可用入口网址且需调用 discover_sources 搜索候选 URL 时置为 true"`
	NeedsCalculation  bool            `json:"needs_calculation,omitempty" jsonschema:"description=用户要求根据输入或来源计算派生数值时使用。直接查询或比较已发布原数时不需要。"`
	Questions         []string        `json:"questions,omitempty"`
	Actions           []PlanAction    `json:"actions,omitempty"`
	CoverageRationale string          `json:"coverage_rationale"`
}

type PlannedSource struct {
	URL      string `json:"url"`
	Route    string `json:"route" jsonschema:"enum=direct,enum=api,enum=lua,enum=asset"`
	Expected string `json:"expected" jsonschema:"description=这个来源提供哪些结果或证据"`
}

type PlanAction struct {
	ID        string `json:"id"`
	Goal      string `json:"goal"`
	Status    string `json:"status"`
	ResultRef string `json:"result_ref,omitempty"`
}

// BusinessError is a correctable tool result. The Eino adapter serializes it
// rather than terminating the runner with it.
type BusinessError struct {
	Code             string         `json:"code"`
	Owner            string         `json:"owner"`
	Recoverability   string         `json:"recoverability"`
	Message          string         `json:"message"`
	Details          map[string]any `json:"details,omitempty"`
	SuggestedActions []string       `json:"suggested_actions"`
	EvidenceRefs     []string       `json:"evidence_refs,omitempty"`
}

func (e *BusinessError) Error() string { return e.Code + ": " + e.Message }
func (e *BusinessError) ToolFeedback() (string, []string, map[string]any) {
	return e.Code, e.SuggestedActions, e.Details
}
func (e *BusinessError) OperationalStatus() agentstatus.Status {
	s := agentstatus.Of(agentstatus.AgentCorrection)
	if e.Owner == "caller" {
		s = agentstatus.Of(agentstatus.InvalidRequest)
	} else if e.Owner == "system" {
		s = agentstatus.Of(agentstatus.SystemDependency)
	}
	s.Recovery = append([]string{}, e.SuggestedActions...)
	return s
}

func businessError(code, owner, message, action string) *BusinessError {
	return &BusinessError{Code: code, Owner: owner, Recoverability: "agent", Message: message, SuggestedActions: []string{action}}
}
