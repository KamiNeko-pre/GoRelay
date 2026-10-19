package aiapp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/agentdata"
)

// Only the provider is replaced. Decisions consume the real serialized request;
// no DB, fixture body, gold values, selectors or private IDs enter this model.
// This deterministic policy tests the tool protocol, not model intelligence.
type crawlerMockMemory struct {
	Root, Goal, Listing, Detail, ExpectedTitle, Anchor, Title, Date, LastRead string
	Catalog                                                                   []DOMBlock
	Next                                                                      *dataReadArgs
	Tried                                                                     map[string]bool
	Links                                                                     []string
	Submission                                                                json.RawMessage
	SourcesChecked                                                            bool
	Evidence                                                                  map[string]fieldLocator
}

func adaptiveCrawlerDecision(req ChatRequest, fault bool) (ChatResponse, error) {
	if len(req.Tools) == 0 {
		return correctChainDecision(req)
	} // evidence-only answer endpoint
	receipts, facts := auditVisibleReceipts(req)
	mem := crawlerMockMemory{Tried: map[string]bool{}}
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Content, "crawler-memory:") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(m.Content, "crawler-memory:")), &mem); err != nil {
				return ChatResponse{}, err
			}
		}
	}
	if mem.Tried == nil {
		mem.Tried = map[string]bool{}
	}
	var spec TaskSpec
	json.Unmarshal(facts["frozen_spec"], &spec)
	if mem.Root == "" {
		for _, m := range req.Messages {
			if m.Role == "user" {
				var in CreateRunRequest
				if json.Unmarshal([]byte(m.Content), &in) == nil {
					mem.Root = regexp.MustCompile(`https?://[^\s"<>]+`).FindString(in.Goal)
					mem.Goal = in.Goal
				}
			}
		}
	}
	propose := func(name string, args any) (ChatResponse, error) {
		available := false
		for _, d := range req.Tools {
			if d.Function.Name == name {
				available = true
				var schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                   `json:"required"`
				}
				json.Unmarshal(d.Function.Parameters, &schema)
				raw, _ := json.Marshal(args)
				var fields map[string]json.RawMessage
				json.Unmarshal(raw, &fields)
				for key := range fields {
					if schema.Properties[key] == nil {
						return ChatResponse{}, fmt.Errorf("mock proposed undocumented %s argument %s", name, key)
					}
				}
				for _, key := range schema.Required {
					if fields[key] == nil {
						return ChatResponse{}, fmt.Errorf("mock omitted required %s argument %s", name, key)
					}
				}
			}
		}
		if !available {
			return ChatResponse{}, fmt.Errorf("mock cannot propose unavailable tool %s", name)
		}
		raw, _ := json.Marshal(args)
		memory, _ := json.Marshal(mem)
		return ChatResponse{Message: Message{Role: "assistant", Content: "crawler-memory:" + string(memory), ReasoningContent: "Use " + name + " to address the current requested fields from the latest visible receipt: " + string(raw), ToolCalls: []ToolCall{{ID: "adaptive-" + agentdata.Hash(req)[:20], Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}}, nil
	}
	if spec.Goal == "" {
		spec.Goal = mem.Goal
	}
	dataOf := func(out ToolResult) map[string]json.RawMessage {
		var data map[string]json.RawMessage
		json.Unmarshal(out.Data, &data)
		if data["observation"] != nil {
			var nested map[string]json.RawMessage
			if json.Unmarshal(data["observation"], &nested) == nil {
				data = nested
			}
		}
		return data
	}
	var coverage CoverageReport
	json.Unmarshal(facts["coverage"], &coverage)
	if coverage.Satisfied {
		if !spec.RequireSummary {
			return propose("finish_task", map[string]any{"status": "completed", "reason": "observed host coverage satisfies the frozen contract"})
		}
		// Retrieval/query text is derived from the actual task, not fixture Gold.
		search := receipts["search_knowledge"]
		var catalog []struct {
			ID string `json:"id"`
		}
		json.Unmarshal(facts["evidence_catalog"], &catalog)
		if search.Name == "" || len(catalog) == 0 {
			return propose("search_knowledge", map[string]any{"query": spec.Goal})
		}
		read := receipts["read_data"]
		d := dataOf(read)
		var handle, text string
		json.Unmarshal(d["handle"], &handle)
		json.Unmarshal(d["text"], &text)
		if strings.HasPrefix(handle, "ev_") && text != "" {
			match := regexp.MustCompile(`预算金额[：:]\s*[0-9]+万元`).FindString(text)
			if match != "" {
				assessments := []RequirementResult{}
				for _, r := range spec.Requirements {
					assessments = append(assessments, RequirementResult{ID: r.ID, Status: "satisfied", Explanation: "retrieved source explicitly states the requested amount", Citations: []string{handle}})
				}
				return propose("finish_task", map[string]any{"status": "completed", "reason": "records and cited source evidence are present", "summary": match, "citations": []string{handle}, "requirements": assessments})
			}
			var next dataReadArgs
			if json.Unmarshal(d["next_read"], &next) == nil && next.Handle != "" {
				return propose("read_data", next)
			}
			return ChatResponse{}, fmt.Errorf("visible evidence has no requested amount; mock cannot invent one")
		}
		return propose("read_data", map[string]any{"handle": catalog[0].ID, "pointer": "/text", "limit": 1000})
	}
	if out := receipts["register_source"]; out.Name != "" && out.Error == "" && len(mem.Submission) > 0 {
		return propose("submit_acquisition", mem.Submission)
	}
	if out := receipts["register_source"]; out.Error != "" && out.Failure != nil && out.Failure.Recovery != nil && out.Failure.Recovery.Tool == "validate_program" {
		recovery := out.Failure.Recovery
		resolved := false
		if condition := recovery.ResolvedBy; condition != nil {
			data := dataOf(receipts[condition.Tool])
			resolved = len(condition.Fields) > 0
			for field, want := range condition.Fields {
				var got string
				json.Unmarshal(data[field], &got)
				if got != want {
					resolved = false
				}
			}
		}
		if !resolved {
			args, err := visibleWorkingProgram(req)
			if err != nil {
				return ChatResponse{}, err
			}
			for _, step := range recovery.Steps {
				if step.Tool == "validate_program" {
					for field, value := range step.ArgumentChanges {
						args[field] = value
					}
				}
			}
			delete(args, "repair_of")
			return propose("validate_program", args)
		}
	}
	if out := receipts["validate_program"]; out.Name != "" {
		if out.Error != "" {
			if out.Failure != nil && out.Failure.Code == "FIELD_EVIDENCE_UNSUPPORTED" {
				args, err := visibleWorkingProgram(req)
				if err != nil {
					return ChatResponse{}, err
				}
				var details struct {
					Fields []struct {
						Field string `json:"field"`
					} `json:"unsupported_fields"`
				}
				raw, _ := json.Marshal(out.Failure.Details)
				json.Unmarshal(raw, &details)
				locators := map[string]fieldLocator{}
				for _, issue := range details.Fields {
					loc, ok := mem.Evidence[issue.Field]
					if !ok {
						return ChatResponse{}, fmt.Errorf("no observed extraction locator for diagnosed field %s", issue.Field)
					}
					locators[issue.Field] = loc
				}
				if len(locators) == 0 {
					return ChatResponse{}, fmt.Errorf("missing field-level correction diagnostics")
				}
				code, ok := args["code"].(string)
				if !ok || code == "" {
					return ChatResponse{}, fmt.Errorf("saved program is not editable")
				}
				proof, _ := json.Marshal(locators)
				args["code"] = "local previous_program=function()\n" + code + "\nend\nlocal ok,value=previous_program(); if not ok then return ok,value end; local locators=assert(json_decode(" + strconv.Quote(string(proof)) + ")); for _,row in ipairs(value.items or {}) do row._evidence=locators end; return true,value"
				args["repair_of"] = out.CallID
				return propose("validate_program", args)
			}
			return ChatResponse{}, fmt.Errorf("mock program rejected: %s", out.Error)
		}
		var kind string
		json.Unmarshal(dataOf(out)["kind"], &kind)
		if spec.RequirePublishedCapability && kind == "composition" {
			args, err := visibleWorkingProgram(req)
			if err != nil {
				return ChatResponse{}, err
			}
			args["kind"], args["mode"] = "adapter", "acquisition"
			delete(args, "repair_of")
			return propose("validate_program", args)
		}
		var next struct {
			Tool       string          `json:"tool"`
			Arguments  json.RawMessage `json:"arguments"`
			Submission json.RawMessage `json:"then_submit"`
		}
		json.Unmarshal(dataOf(out)["next_action"], &next)
		if next.Tool == "" || len(next.Submission) == 0 {
			return ChatResponse{}, fmt.Errorf("validated receipt lacks continuation")
		}
		mem.Submission = next.Submission
		return propose(next.Tool, next.Arguments)
	}
	if receipts["list_sources"].Name == "" && !mem.SourcesChecked {
		return propose("list_sources", map[string]any{})
	}
	if receipts["list_sources"].Name != "" {
		mem.SourcesChecked = true
	}
	var active []SourceCapability
	json.Unmarshal(dataOf(receipts["list_sources"])["active"], &active)
	for _, c := range active {
		if c.DirectUseIssue == "" {
			var params map[string]any
			json.Unmarshal([]byte(c.InputExampleJSON), &params)
			return propose("submit_acquisition", map[string]any{"source_id": c.Source.SourceID, "url": mem.Root, "params": params})
		}
	}
	if mem.Root == "" {
		return ChatResponse{}, fmt.Errorf("task supplied no observed entry URL")
	}
	inspection := receipts["inspect_source"]
	if mem.Listing == "" {
		if inspection.Name == "" {
			return propose("inspect_source", map[string]any{"url": mem.Root})
		}
		if inspection.Error != "" {
			return ChatResponse{}, fmt.Errorf("mock fetch rejected: %s", inspection.Error)
		}
		mem.Listing = inspection.CallID
	}
	if len(mem.Links) > 0 && mem.Detail == "" {
		if inspection.CallID == mem.Listing {
			return propose("inspect_source", map[string]any{"url": mem.Links[0]})
		}
		mem.Detail = inspection.CallID
		mem.Catalog = nil
		mem.Next = nil
		mem.LastRead = ""
	}
	handle := mem.Listing
	if mem.Detail != "" {
		handle = mem.Detail
	}
	read := receipts["read_data"]
	if read.Name != "" && read.CallID != mem.LastRead {
		mem.LastRead = read.CallID
		if read.Error != "" {
			// Correct the actual rejected call against its structured diagnostic.
			if read.Failure == nil {
				return ChatResponse{}, fmt.Errorf("unstructured read failure: %s", read.Error)
			}
			for _, m := range req.Messages {
				for _, call := range m.ToolCalls {
					if call.ID == read.CallID {
						var args dataReadArgs
						if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
							return ChatResponse{}, fmt.Errorf("missing rejected arguments")
						}
						args.View = "dom"
						args.Pointer = ""
						return propose("read_data", args)
					}
				}
			}
			return ChatResponse{}, fmt.Errorf("rejected read not present in current request")
		}
		d := dataOf(read)
		var h string
		json.Unmarshal(d["handle"], &h)
		if h == handle {
			var entries []DOMBlock
			json.Unmarshal(d["selector_catalog"], &entries)
			mem.Catalog = append(mem.Catalog, entries...)
			mem.Next = nil
			var next dataReadArgs
			if json.Unmarshal(d["next_read"], &next) == nil && next.Handle != "" {
				mem.Next = &next
			}
			if mem.Detail == "" {
				var nodes []struct {
					Text       string                        `json:"text"`
					Attributes map[string]string             `json:"attributes"`
					Links      []struct{ Href, Text string } `json:"links"`
				}
				json.Unmarshal(d["nodes"], &nodes)
				for _, n := range nodes {
					if href := n.Attributes["href"]; href != "" {
						mem.Links = append(mem.Links, href)
						if mem.ExpectedTitle == "" {
							mem.ExpectedTitle = n.Text
						}
					}
					for _, link := range n.Links {
						mem.Links = append(mem.Links, link.Href)
						if mem.ExpectedTitle == "" {
							mem.ExpectedTitle = link.Text
						}
					}
				}
				if len(mem.Links) > 0 {
					var selector, sample string
					json.Unmarshal(d["selector"], &selector)
					json.Unmarshal(d["html_sample"], &sample)
					doc, _ := goquery.NewDocumentFromReader(strings.NewReader(sample))
					if node := doc.Find("[href]").First(); node.Length() > 0 {
						if len(nodes) > 0 && nodes[0].Attributes["href"] != "" {
							mem.Anchor = selector
						} else {
							mem.Anchor = selector + " " + node.Nodes[0].Data + "[href]"
						}
					}
					if mem.Anchor == "" {
						return ChatResponse{}, fmt.Errorf("link locator absent from observed structure")
					}
					return propose("inspect_source", map[string]any{"url": mem.Links[0]})
				}
			}
		}
	}
	if mem.Detail != "" {
		for _, b := range mem.Catalog {
			if b.Sample == mem.ExpectedTitle && b.Matches == 1 && (mem.Title == "" || len(b.Selector) < len(mem.Title)) {
				mem.Title = b.Selector
			}
			if regexp.MustCompile(`\d{4}-\d{2}-\d{2}`).MatchString(b.Sample) && b.Matches == 1 && (mem.Date == "" || len(b.Selector) < len(mem.Date)) {
				mem.Date = b.Selector
			}
		}
		if mem.Title != "" && mem.Date != "" {
			mem.Evidence = map[string]fieldLocator{"title": {Selector: mem.Title, Index: 0}, "published_date": {Selector: mem.Date, Index: 0, Pattern: "([0-9]{4}-[0-9]{2}-[0-9]{2})"}}
			// Synthesize a loop from observed locators; no fixture-specific Lua.
			q := strconv.Quote
			code := `local h=assert(http_get(TARGET_URL));local refs=assert(html_attr_all(h,` + q(mem.Anchor) + `,"href"));local items={};for i=1,math.min(PARAMS.detail_limit,#refs) do local u=url_resolve(TARGET_URL,refs[i]);local d=assert(http_get(u));local title=assert(html_find(d,` + q(mem.Title) + `));local meta=assert(html_find(d,` + q(mem.Date) + `));local date=assert(meta:match("(%d%d%d%d%-%d%d%-%d%d)"));table.insert(items,{title=title,published_date=date,body=assert(source_text(d)),source_url=u,_evidence={title={selector=` + q(mem.Title) + `,index=0},published_date={selector=` + q(mem.Date) + `,index=0,pattern="([0-9]{4}-[0-9]{2}-[0-9]{2})"},body={source_text=true}}}) end;return true,{items=items}`
			return propose("validate_program", map[string]any{"code": code, "kind": "adapter", "mode": "acquisition", "source_id": "source-" + agentdata.Hash(mem.Root)[:16], "purpose": spec.Goal, "url": mem.Root, "params": map[string]any{"detail_limit": spec.MinRecords}})
		}
	}
	for _, b := range mem.Catalog {
		key := handle + "|" + b.Selector
		if mem.Detail == "" && b.Matches > 1 && !mem.Tried[key] {
			mem.Tried[key] = true
			args := dataReadArgs{Handle: handle, View: "dom", Selector: b.Selector}
			if fault && !mem.Tried["injected"] {
				mem.Tried["injected"] = true
				args.View = "json"
				args.Pointer = "/items"
			}
			return propose("read_data", args)
		}
	}
	if mem.Next != nil {
		next := *mem.Next
		mem.Next = nil
		return propose("read_data", next)
	}
	if len(mem.Catalog) == 0 {
		return propose("read_data", dataReadArgs{Handle: handle, View: "dom", Limit: 3})
	}
	return ChatResponse{}, fmt.Errorf("observed inventory exhausted without required locators")
}

// This client has no private copy of code/locators. Like the real provider it
// can repair a program only from an actual assistant proposal or host snapshot.
func visibleWorkingProgram(req ChatRequest) (map[string]any, error) {
	var args map[string]any
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.Function.Name == "validate_program" {
					var proposed map[string]any
					if json.Unmarshal([]byte(call.Function.Arguments), &proposed) == nil && proposed["code"] != "" && proposed["code"] != nil {
						args = proposed
					}
				}
			}
		}
		if message.Role == "tool" {
			var snapshot struct {
				State struct {
					Programs []programCorrection `json:"program_corrections"`
				} `json:"crawler_state"`
			}
			if json.Unmarshal([]byte(message.Content), &snapshot) == nil {
				for _, program := range snapshot.State.Programs {
					json.Unmarshal(program.PreviousArguments, &args)
				}
			}
		}
	}
	if args == nil {
		return nil, fmt.Errorf("provider has no visible editable program; cannot invent a repair")
	}
	return args, nil
}

func TestAdaptiveCrawlerRecoversPublicationAfterCompressionFullChain(t *testing.T) {
	for _, variant := range []int{1, 2} {
		t.Run(fmt.Sprint(variant), func(t *testing.T) {
			runCorrectModelSubsystemChain(t, false, 1, false, "", progressiveCase{Name: fmt.Sprintf("publication-recovery-%d", variant), Fault: "wrong_program_kind", ActualRunner: true, SelectorDiscovery: true, CompleteText: true, AdaptiveCrawler: true, LayoutVariant: variant, Expected: "completed"})
		})
	}
}

func TestAdaptiveCrawlerRepairsFieldEvidenceWithSavedProgramFullChain(t *testing.T) {
	for _, variant := range []int{1, 2} {
		t.Run(fmt.Sprint(variant), func(t *testing.T) {
			runCorrectModelSubsystemChain(t, false, 1, false, "", progressiveCase{Name: fmt.Sprintf("field-evidence-recovery-%d", variant), Fault: "missing_field_evidence", ActualRunner: true, SelectorDiscovery: true, CompleteText: true, AdaptiveCrawler: true, LayoutVariant: variant, Expected: "completed"})
		})
	}
}

func TestAdaptiveCrawlerUsesOnlyObservedSelectors(t *testing.T) {
	for _, selector := range []string{".random_17 > section", "#other89 > div"} {
		memory, _ := json.Marshal(crawlerMockMemory{Root: "https://site.example/list", Listing: "fresh-handle", Tried: map[string]bool{}})
		data, _ := json.Marshal(map[string]any{"handle": "fresh-handle", "selector_catalog": []DOMBlock{{Selector: selector, Matches: 4, Sample: "observed rows"}}})
		req := feedbackRequest(map[string]any{}, ToolResult{Name: "list_sources", Data: json.RawMessage(`{"active":[]}`)}, ToolResult{Name: "read_data", CallID: "catalog", Data: data})
		req.Messages = append(req.Messages, Message{Role: "assistant", Content: "crawler-memory:" + string(memory)})
		out, err := adaptiveCrawlerDecision(req, false)
		if err != nil {
			t.Fatal(err)
		}
		var args dataReadArgs
		json.Unmarshal([]byte(out.Message.ToolCalls[0].Function.Arguments), &args)
		if args.Handle != "fresh-handle" || args.Selector != selector {
			t.Fatalf("selector was not derived from the visible inventory: %+v", out)
		}
	}
}

func TestAdaptiveCrawlerNormalEntryFullChain(t *testing.T) {
	for _, variant := range []int{1, 2} {
		t.Run(fmt.Sprint(variant), func(t *testing.T) {
			runCorrectModelSubsystemChain(t, false, 2, false, "", progressiveCase{Name: fmt.Sprintf("adaptive-crawler-%d", variant), ActualRunner: true, SelectorDiscovery: true, CompleteText: true, AdaptiveCrawler: true, LayoutVariant: variant, Expected: "completed"})
		})
	}
}
