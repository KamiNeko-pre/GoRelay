package aiapp

import (
	"fmt"
	"strings"
)

func crawlerPlanFailure(spec TaskSpec) error {
	fields := []string{}
	actual := map[string]any{}
	if spec.RequireSummary {
		fields = append(fields, "require_summary")
		actual["require_summary"] = true
	}
	if len(spec.Requirements) > 0 {
		fields = append(fields, "requirements")
		actual["requirements"] = spec.Requirements
	}
	if len(fields) == 0 {
		return nil
	}
	return toolContractFailure(fmt.Errorf("crawler plan parameters %s request knowledge analysis outside the acquisition contract", strings.Join(fields, ", ")), "CRAWLER_CONTRACT_ONLY", "not_started", map[string]any{"field": fields[0], "fields": fields, "actual": actual, "accepted": map[string]any{"require_summary": false, "requirements": []TaskRequirement{}}}, ToolRecovery{Strategy: "correct_arguments", Tool: "plan_task", Instruction: "Resubmit plan_task with require_summary=false and requirements omitted or empty. Preserve the user goal, all acquisition fields, entities and min_records. Express requested output values as required_fields, exact row selection as required_combinations, and relational identity as join_keys; implement other acquisition predicates in Lua and validate their actual source evidence. Knowledge analysis belongs to the separate workflow. Changing require_summary alone does not fix nonempty requirements."})
}
