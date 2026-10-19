// agent-eval runs independent offline validator probes. Fixture ground truth is
// confined to this evaluation executable, never passed to runtime Agent tools.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"os"
	"strings"
	"time"
)

const first = "https://fixture.example/releases?page=1"
const second = "https://fixture.example/releases?page=2"
const healthy = `local records={};local current=TARGET_URL
while current and current~="" do
 local body,err=http_get(current);assert(body,err)
 local data=assert(json_decode(body))
 for _,item in ipairs(data.rows) do table.insert(records,{name=item.name,version=item.version,source_url=current}) end
 current=data.next
end
return true,{items=records}`

type probe struct {
	Name               string `json:"name"`
	StructuralAccepted bool   `json:"structural_accepted"`
	GoalCoverage       bool   `json:"goal_coverage_satisfied"`
	GoldCorrect        bool   `json:"independent_gold_correct"`
	Requests           int    `json:"http_requests"`
	DurationMS         int64  `json:"duration_ms"`
	Error              string `json:"error,omitempty"`
}

func audit() ([]probe, error) {
	spec := aiapp.TaskSpec{Goal: "collect Alpha and Beta releases", SchemaVersion: "fixture-v1", Entities: []string{"Alpha", "Beta"}, EntityField: "name", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}}
	entries := []acquisition.ReplayEntry{{Method: "GET", URL: first, Status: 200, Body: `{"rows":[{"name":"Alpha","version":"1.0"}],"next":"` + second + `"}`, FetchedAt: time.Now()}, {Method: "GET", URL: second, Status: 200, Body: `{"rows":[{"name":"Beta","version":"2.0"}]}`, FetchedAt: time.Now()}}
	variants := []struct {
		name, code string
		layout     bool
	}{{"healthy_control", healthy, false}, {"changed_layout", healthy, true}, {"wrong_column_same_type", strings.ReplaceAll(healthy, "version=item.version", "version=item.name"), false}, {"missing_page", strings.ReplaceAll(healthy, "current=data.next", "current=nil"), false}, {"value_not_supported_by_evidence", strings.ReplaceAll(healthy, "version=item.version", `version="99.99"`), false}}
	out := []probe{}
	for _, variant := range variants {
		fixtures := append([]acquisition.ReplayEntry{}, entries...)
		if variant.layout {
			fixtures[0].Body = strings.ReplaceAll(fixtures[0].Body, `"rows"`, `"entries"`)
		}
		replay := acquisition.NewReplay(fixtures)
		result, err := engine.RunProgram(context.Background(), engine.ProgramRequest{Code: variant.code, TargetURL: first}, engine.DefaultProgramLimits(), replay.Fetch)
		p := probe{Name: variant.name, Requests: result.Stats.Requests, DurationMS: result.DurationMS}
		if err != nil {
			p.Error = err.Error()
			out = append(out, p)
			continue
		}
		records, err := aiapp.BindRecords(result, aiapp.RecordBinding{SourceID: "fixture-source", DataVersion: "fixture-code", TaskID: 1, SchemaVersion: spec.SchemaVersion, KeyFields: spec.JoinKeys})
		if err != nil {
			return nil, err
		}
		facts, err := aiapp.EvaluateRecords(spec, records, time.Now())
		if err != nil {
			return nil, err
		}
		p.StructuralAccepted = len(records) > 0 && len(facts.Issues) == 0
		p.GoalCoverage = facts.Satisfied
		gold := map[string]string{"Alpha": "1.0", "Beta": "2.0"}
		seen := map[string]bool{}
		p.GoldCorrect = len(records) == len(gold)
		for _, row := range records {
			name, _ := row.Fields["name"].(string)
			version, _ := row.Fields["version"].(string)
			if gold[name] != version || seen[name] {
				p.GoldCorrect = false
			}
			seen[name] = true
		}
		out = append(out, p)
	}
	return out, nil
}
func main() {
	path := flag.String("output", "", "optional JSON output file")
	mode := flag.String("mode", "validator", "validator / tasks / summarize / refresh / diff / costs")
	manifest := flag.String("manifest", "", "public frozen task manifest")
	gold := flag.String("gold", "", "private independent field truth, never sent to Agent")
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080", "application API endpoint")
	batch := flag.String("batch", "", "stable batch ID for restart")
	variant := flag.String("variant", "code", "experiment variant label; configure identical app variant separately")
	repetitions := flag.Int("repetitions", 1, "independent initial states must be prepared separately")
	trials := flag.String("trials", "", "JSONL input for summarize")
	pricing := flag.String("pricing", "", "dated provider rate JSON for costs; no guessed defaults")
	before := flag.String("before", "", "business diff: prior TaskResult JSON")
	after := flag.String("after", "", "business diff: current TaskResult JSON")
	keys := flag.String("keys", "", "business diff: comma-separated business identity fields")
	flag.Parse()
	if *mode != "validator" {
		var err error
		switch *mode {
		case "diff":
			err = exportBusinessDiff(*before, *after, *keys, *gold, *path)
		case "tasks":
			err = runTasks(*endpoint, *manifest, *gold, *batch, *variant, *path, *repetitions)
		case "summarize":
			err = summarizeTrials(*trials, *path)
		case "costs":
			err = exportCosts(*trials, *pricing, *path)
		case "refresh":
			err = refreshTrials(*trials, *endpoint, *path)
		default:
			err = fmt.Errorf("unknown mode")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	rows, err := audit()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	report := map[string]any{"mode": "offline_fixtures", "live_agent_evaluated": false, "registered_source_evaluated": false, "semantic_gold_in_runtime": false, "cases": rows, "limitations": "Receipt/schema checks cannot prove field meaning; wrong column or unsupported values can pass. Missing pages must be judged against frozen goal/entity/page requirements."}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}
	if *path != "" {
		if err = os.WriteFile(*path, append(raw, '\n'), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else {
		fmt.Println(string(raw))
	}
}
