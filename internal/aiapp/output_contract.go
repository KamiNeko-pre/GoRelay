package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"time"
)

// applicableCapabilities lists the active capabilities worth showing this run.
//
// The catalog serves two purposes and they pull in opposite directions:
//
//   - discovery: an existing program whose field names differ from this task's
//     is still useful, because the caller may adapt it rather than write a new
//     one. Hiding it makes the run redo work that already exists.
//   - honesty: a program that cannot serve this task even after adaptation is
//     noise that invites a failed attempt.
//
// So the catalog shows what is adaptable and hides only what is unusable: a
// capability missing a required join key, declaring a conflicting type for a
// field this task also names, or reaching outside the granted network scope.
// Missing field-name overlap is not a reason to hide, because renaming a field
// is exactly the adaptation a caller can perform.
//
// Direct execution stays strict: checkCapabilityForRun is unchanged and still
// refuses to submit an incompatible program. Listing a candidate for adaptation
// never weakens what may be executed.
func (s *Service) applicableCapabilities(ctx context.Context, r agentdata.Run) ([]SourceCapability, error) {
	all, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if r.SpecJSON == "" {
		return all, nil
	}
	spec, err := specOf(r)
	if err != nil {
		return all, nil
	}
	wanted := map[string]string{}
	for _, f := range spec.RequiredFields {
		wanted[f.Name] = f.Type
	}
	granted := scope(r)
	out := []SourceCapability{}
	for _, c := range all {
		artifact, err := s.Data.Artifact(ctx, c.Source.ArtifactHash)
		if err != nil {
			continue
		}
		if !capabilityAdaptable(spec, wanted, granted, artifact) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// capabilityAdaptable reports whether an existing program could serve this task
// after a reasonable adaptation. It is deliberately weaker than
// checkCapabilityForRun: field-name differences are allowed, but a genuine
// contradiction is not.
func capabilityAdaptable(spec TaskSpec, wanted map[string]string, granted []string, a agentdata.ProgramArtifact) bool {
	var output TaskSpec
	if err := json.Unmarshal([]byte(a.OutputSchema), &output); err != nil {
		return false
	}
	if output.SchemaVersion != spec.SchemaVersion {
		return false
	}
	available := map[string]string{}
	for _, f := range output.RequiredFields {
		available[f.Name] = f.Type
	}
	// A join key must be present: no adaptation can invent the value that links
	// this source's rows to the rest of the task.
	for _, key := range spec.JoinKeys {
		if _, ok := available[key]; !ok {
			return false
		}
	}
	// A same-named field with a conflicting type is a real contradiction; a
	// differently-named field is a rename the caller can perform.
	for name, declared := range available {
		if target, ok := wanted[name]; ok && target != declared && !(declared == "integer" && target == "number") {
			return false
		}
	}
	// The program must stay inside the scope this run was granted.
	var domains []string
	if json.Unmarshal([]byte(a.ScopeJSON), &domains) != nil {
		return false
	}
	if len(granted) > 0 {
		if len(domains) == 0 {
			return false
		}
		for _, domain := range domains {
			if !admissionHostAllowed(domain, granted) {
				return false
			}
		}
	}
	return true
}

// A reusable capability may supply part of a task and additional columns, but
// its declared types and the task's join keys must be usable without guessing
// a field mapping. This is applicability, not a judgment that a source broke.
func checkCapabilityForRun(r agentdata.Run, a agentdata.ProgramArtifact) error {
	if r.SpecJSON == "" {
		return nil
	} // The planner may inspect the catalog first.
	spec, err := specOf(r)
	if err != nil {
		return err
	}
	var output TaskSpec
	if err := json.Unmarshal([]byte(a.OutputSchema), &output); err != nil {
		return fmt.Errorf("invalid capability output contract: %w", err)
	}
	if err := output.Validate(); err != nil {
		return err
	}
	if output.SchemaVersion != spec.SchemaVersion {
		return fmt.Errorf("capability record schema version differs")
	}
	wanted, available := map[string]string{}, map[string]string{}
	for _, f := range spec.RequiredFields {
		wanted[f.Name] = f.Type
	}
	matched := false
	for _, f := range output.RequiredFields {
		available[f.Name] = f.Type
		if target, ok := wanted[f.Name]; ok {
			if target != f.Type && !(f.Type == "integer" && target == "number") {
				return fmt.Errorf("capability field type differs: %s", f.Name)
			}
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("capability provides no requested fields")
	}
	for _, key := range spec.JoinKeys {
		if available[key] == "" {
			return fmt.Errorf("capability lacks task join key: %s", key)
		}
	}
	var domains []string
	if err := json.Unmarshal([]byte(a.ScopeJSON), &domains); err != nil {
		return fmt.Errorf("invalid capability network scope: %w", err)
	}
	granted := scope(r)
	if len(granted) > 0 {
		if len(domains) == 0 {
			return fmt.Errorf("unrestricted capability exceeds task network scope; retrial required")
		}
		for _, domain := range domains {
			if !admissionHostAllowed(domain, granted) {
				return fmt.Errorf("capability exceeds task network scope; retrial required")
			}
		}
	}
	return nil
}

// Per-source production promises are subsets; the run's delivery contract stays
// unchanged. Join keys must be acquired by every complementary source.
func OutputContract(spec TaskSpec, fields []string) (TaskSpec, error) {
	out := spec
	out.Entities = nil
	out.EntityField = ""
	out.MinRecords = 0
	out.RequiredCombinations = nil
	out.RequirePublishedCapability = false
	// Host field support is checked for every produced field before admission
	// and publication. A reusable subset must not weaken that frozen promise.
	if len(fields) == 0 {
		return out, out.Validate()
	}
	selected := map[string]bool{}
	for _, f := range fields {
		if selected[f] {
			return TaskSpec{}, outputDeclarationFailure(spec, fields, fmt.Errorf("duplicate produced field"))
		}
		selected[f] = true
	}
	out.RequiredFields = nil
	for _, f := range spec.RequiredFields {
		if selected[f.Name] {
			out.RequiredFields = append(out.RequiredFields, f)
			delete(selected, f.Name)
		}
	}
	if len(selected) > 0 {
		return out, outputDeclarationFailure(spec, fields, fmt.Errorf("produced field outside frozen requirements"))
	}
	for _, key := range spec.JoinKeys {
		found := false
		for _, f := range out.RequiredFields {
			if f.Name == key {
				found = true
			}
		}
		if !found {
			return out, outputDeclarationFailure(spec, fields, fmt.Errorf("source must produce join key %s", key))
		}
	}
	return out, out.Validate()
}

func outputDeclarationFailure(spec TaskSpec, fields []string, cause error) error {
	return toolContractFailure(cause, "OUTPUT_DECLARATION_ERROR", "not_started",
		map[string]any{"required_fields": spec.RequiredFields, "declared_fields": fields, "join_keys": spec.JoinKeys},
		ToolRecovery{Strategy: "correct_arguments", Instruction: "Declare requested fields with their required join keys for acquisition. If the program only explores intermediate structure, use the existing probe mode; probe output is not a final delivery."})
}

// A source may legitimately produce only complementary facts. Explain that
// existing contract instead of ordering the model to invent absent fields.
func programOutputFailure(spec TaskSpec, declared []string, records []Record, coverage CoverageReport) error {
	present := []string{}
	for _, f := range spec.RequiredFields {
		all := len(records) > 0
		for _, record := range records {
			if value, ok := record.Fields[f.Name]; !ok || value == nil {
				all = false
				break
			}
		}
		if all {
			present = append(present, f.Name)
		}
	}
	details := map[string]any{"required_fields": spec.RequiredFields, "declared_fields": declared, "join_keys": spec.JoinKeys, "record_count": len(records), "coverage": coverage}
	recovery := ToolRecovery{Strategy: "correct_program", Tool: "validate_program", Instruction: "The program executed but did not satisfy its declared output contract. Correct the reported missing or invalid fields using observed structure. When sources provide complementary fields, declare produced_fields for each source including all join keys, validate and submit each subset, then verify the full task coverage after merging. Do not invent the fields a source lacks."}
	if subset, err := OutputContract(spec, present); err == nil && len(present) > 0 && len(present) < len(spec.RequiredFields) {
		if checked, err := EvaluateRecords(subset, records, time.Now()); err == nil && len(checked.Issues) == 0 && len(records) > 0 {
			details["supported_subset_fields"] = present
			recovery.Steps = []RecoveryStep{{Action: "declare_verified_source_subset", Tool: "validate_program", ArgumentChanges: map[string]any{"produced_fields": present, "mode": "acquisition"}, Instruction: "Reuse the working program and declare these host-verified fields; the full task contract remains unchanged."}, {Action: "submit_complementary_facts", Tool: "submit_acquisition", Instruction: "Submit the successful candidate and acquire the other required fields from their observed sources; host joins complementary facts by the frozen join keys."}}
		}
	}
	return toolContractFailure(fmt.Errorf("trial has empty/invalid records"), "PROGRAM_OUTPUT_CONTRACT_ERROR", "failed", details, recovery)
}
