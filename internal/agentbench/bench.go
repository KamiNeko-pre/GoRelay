// Package agentbench evaluates outputs outside the Agent runtime. Private gold
// never enters prompts, tools, source adapters or their registration checks.
package agentbench

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/information"
	"go-lua-crawler/internal/knowledge"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Case struct {
	Spec           *aiapp.TaskSpec `json:"spec,omitempty"`
	ID             string          `json:"id"`
	Goal           string          `json:"goal"`
	Split          string          `json:"split"`
	Solvable       bool            `json:"solvable"`
	SelectedBy     string          `json:"selected_by"`
	SourceScope    []string        `json:"source_scope,omitempty"`
	EntryURLs      []string        `json:"entry_urls,omitempty"`
	TimeoutSeconds *int64          `json:"timeout_seconds,omitempty"`
}
type Manifest struct {
	EntryPoint                string                        `json:"entry_point,omitempty"`
	Status                    string                        `json:"status"`
	EnvironmentID             string                        `json:"environment_id"`
	Mode                      string                        `json:"mode"`
	Build                     string                        `json:"build"`
	Model                     string                        `json:"model"`
	InitialState              string                        `json:"initial_state"`
	RepetitionPolicy          string                        `json:"repetition_policy"`
	ExpectedActionMode        string                        `json:"expected_action_mode"`
	ExpectedInformationPolicy *information.ExperimentPolicy `json:"expected_information_policy,omitempty"`
	Cases                     []Case                        `json:"cases"`
}

func (m Manifest) Validate() error {
	if m.EntryPoint != "" && m.EntryPoint != "crawler" && m.EntryPoint != "information" {
		return fmt.Errorf("unsupported benchmark entry point")
	}
	if m.Status != "frozen" || m.EnvironmentID == "" || len(m.Cases) == 0 {
		return fmt.Errorf("freeze a nonempty manifest with environment_id before execution")
	}
	seen := map[string]bool{}
	for _, c := range m.Cases {
		if c.ID == "" || seen[c.ID] || c.Goal == "" || (c.Split != "development" && c.Split != "holdout") {
			return fmt.Errorf("invalid/duplicate case %s", c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}
func PublicRequest(c Case, key string) aiapp.CreateRunRequest {
	return aiapp.CreateRunRequest{Goal: c.Goal, SourceScope: c.SourceScope, EntryURLs: c.EntryURLs, Spec: c.Spec, TimeoutSeconds: c.TimeoutSeconds, RequestKey: key}
}

type GoldCase struct {
	Verification   string           `json:"verification"` // pending / assistant_prepared / human_confirmed
	ExpectedStatus string           `json:"expected_status"`
	Rows           []map[string]any `json:"rows"`
	KeyFields      []string         `json:"key_fields,omitempty"`
	AllowedDomains []string         `json:"allowed_domains"`
	MaxAgeSeconds  int64            `json:"max_age_seconds"`
	EvidenceNotes  []string         `json:"evidence_notes"`
}
type Judgment struct {
	Correct         *bool    `json:"correct"`
	FalseCompletion bool     `json:"false_completion"`
	Reasons         []string `json:"reasons"`
	MatchedFields   int      `json:"matched_fields"`
	RequiredFields  int      `json:"required_fields"`
	DeliveredFields int      `json:"delivered_fields"`
	FieldScored     bool     `json:"field_scored"`
	FieldCoverage   *float64 `json:"field_coverage"`
	FieldPrecision  *float64 `json:"field_precision"`
}

func equalValue(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && string(x) == string(y)
}
func Judge(gold GoldCase, status string, result aiapp.TaskResult, now time.Time) Judgment {
	j := Judgment{Reasons: []string{}}
	if gold.Verification != "assistant_prepared" && gold.Verification != "human_confirmed" {
		j.Reasons = append(j.Reasons, "independent gold not prepared")
		return j
	}
	correct := true
	j.Correct = &correct
	if status != gold.ExpectedStatus {
		j.Reasons = append(j.Reasons, "unexpected terminal status")
		correct = false
	}
	if gold.ExpectedStatus == "completed" || gold.ExpectedStatus == "partial" {
		if len(gold.Rows) == 0 {
			j.Correct = nil
			j.Reasons = append(j.Reasons, "gold has no delivered facts")
			return j
		}
		if len(result.Records) != len(gold.Rows) {
			correct = false
			j.Reasons = append(j.Reasons, "record count differs from independent gold")
		}
		used := map[int]bool{}
		goldKeys := map[string]bool{}
		actualKeys := map[string]int{}
		if len(gold.KeyFields) > 0 {
			for _, row := range gold.Rows {
				key, err := BusinessKey(row, gold.KeyFields)
				if err != nil || goldKeys[key] {
					j.Correct = nil
					j.Reasons = append(j.Reasons, "invalid/duplicate independent gold business key")
					return j
				}
				goldKeys[key] = true
			}
			for _, row := range result.Records {
				if key, err := BusinessKey(row.Fields, gold.KeyFields); err == nil {
					actualKeys[key]++
				}
			}
		}
		for _, expected := range gold.Rows {
			// Valid gold is required before zero can be a measured result.
			j.FieldScored = true
			j.RequiredFields += len(expected)
			best, bestScore := -1, -1
			expectedKey, _ := BusinessKey(expected, gold.KeyFields)
			for n, actual := range result.Records {
				if used[n] {
					continue
				}
				if len(gold.KeyFields) > 0 {
					key, err := BusinessKey(actual.Fields, gold.KeyFields)
					if err != nil || key != expectedKey || actualKeys[key] != 1 {
						continue
					}
				}
				score := 0
				for field, value := range expected {
					if equalValue(actual.Fields[field], value) {
						score++
					}
				}
				if score > bestScore {
					best, bestScore = n, score
				}
			}
			if best < 0 {
				correct = false
				j.Reasons = append(j.Reasons, "missing/duplicate business identity")
				continue
			}
			used[best] = true
			for field, value := range expected {
				actual := result.Records[best]
				if !equalValue(actual.Fields[field], value) {
					correct = false
					j.Reasons = append(j.Reasons, "field truth mismatch: "+field)
					continue
				}
				supported := false
				for _, source := range actual.FieldSources[field] {
					u, err := url.Parse(source.URL)
					if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
						continue
					}
					allowed := len(gold.AllowedDomains) == 0
					for _, domain := range gold.AllowedDomains {
						if strings.EqualFold(u.Hostname(), domain) {
							allowed = true
						}
					}
					fresh := gold.MaxAgeSeconds == 0 || (!source.FetchedAt.IsZero() && source.FetchedAt.After(now.Add(-time.Duration(gold.MaxAgeSeconds)*time.Second)) && !source.FetchedAt.After(now.Add(time.Minute)))
					if allowed && fresh && source.DataVersion != "" {
						supported = true
					}
				}
				if !supported {
					correct = false
					j.Reasons = append(j.Reasons, "source/freshness missing: "+field)
					continue
				}
				j.MatchedFields++
			}
		}
		if len(result.Conflicts) > 0 {
			correct = false
			j.Reasons = append(j.Reasons, "unresolved field conflicts")
		}
	}
	j.FalseCompletion = status == "completed" && !correct
	if j.FieldScored {
		for _, record := range result.Records {
			j.DeliveredFields += len(record.Fields)
		}
		j.FieldCoverage = fieldRatio(j.MatchedFields, j.RequiredFields)
		j.FieldPrecision = fieldRatio(j.MatchedFields, j.DeliveredFields)
	}
	return j
}

func fieldRatio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	ratio := float64(numerator) / float64(denominator)
	return &ratio
}

type Trial struct {
	CaseID                 string                         `json:"case_id"`
	Repetition             int                            `json:"repetition"`
	Variant                string                         `json:"variant"`
	Mode                   string                         `json:"mode"`
	EnvironmentID          string                         `json:"environment_id"`
	Split                  string                         `json:"split"`
	Solvable               bool                           `json:"solvable"`
	RepetitionPolicy       string                         `json:"repetition_policy"`
	ApplicationActionMode  string                         `json:"application_action_mode"`
	RunID                  string                         `json:"run_id"`
	RequestID              string                         `json:"request_id,omitempty"`
	InformationAccounting  *information.RequestAccounting `json:"information_accounting,omitempty"`
	Status                 string                         `json:"status"`
	Correct                *bool                          `json:"correct"`
	FalseCompletion        bool                           `json:"false_completion"`
	DurationMS             int64                          `json:"duration_ms"`
	RunDurationMS          *int64                         `json:"run_duration_ms,omitempty"`
	PollIntervalMS         int64                          `json:"poll_interval_ms"`
	HumanInterventions     int                            `json:"human_interventions"`
	InterventionAssessment aiapp.InterventionAssessment   `json:"intervention_assessment"`
	MatchedFields          int                            `json:"matched_fields"`
	RequiredFields         int                            `json:"required_fields"`
	DeliveredFields        int                            `json:"delivered_fields"`
	FieldScored            bool                           `json:"field_scored"`
	FieldCoverage          *float64                       `json:"field_coverage"`
	FieldPrecision         *float64                       `json:"field_precision"`
	GoldVerification       string                         `json:"gold_verification"`
	Reasons                []string                       `json:"reasons"`
	Operations             []agentdata.Operation          `json:"operations"`
	Loop                   aiapp.LoopReport               `json:"loop"`
	Snapshot               json.RawMessage                `json:"snapshot"`
	Accounting             *aiapp.RunAccounting           `json:"accounting"`
}
type Summary struct {
	FieldScoredTrials               int            `json:"field_scored_trials"`
	MatchedFields                   int            `json:"matched_fields"`
	RequiredFields                  int            `json:"required_fields"`
	DeliveredFields                 int            `json:"delivered_fields"`
	FieldCoverage                   *float64       `json:"field_coverage"`
	FieldPrecision                  *float64       `json:"field_precision"`
	Mode                            string         `json:"mode"`
	Variant                         string         `json:"variant"`
	EnvironmentID                   string         `json:"environment_id"`
	Trials                          int            `json:"trials"`
	ScoredTrials                    int            `json:"scored_trials"`
	CorrectTasks                    int            `json:"correct_tasks"`
	CorrectVerdicts                 int            `json:"correct_verdicts"`
	FalseCompletions                int            `json:"false_completions"`
	ReportedTokens                  int64          `json:"reported_model_tokens"`
	SearchModelTokens               int64          `json:"reported_search_model_tokens"`
	LLMReportedTokens               int64          `json:"reported_llm_total_tokens"`
	LLMCachedPromptTokens           int64          `json:"reported_llm_cached_prompt_tokens"`
	ReasoningTokens                 int64          `json:"reported_reasoning_tokens_subset"`
	MissingReasoningDetails         int            `json:"missing_reasoning_details_calls"`
	SearchModelAttempts             int            `json:"search_model_attempts"`
	UnknownSearchModelUsage         int            `json:"unknown_search_model_usage_attempts"`
	ModelAttempts                   int            `json:"model_attempts"`
	SearchAttempts                  int            `json:"search_attempts"`
	UnknownUsageAttempts            int            `json:"unknown_model_usage_attempts"`
	EmbeddingAttempts               int            `json:"embedding_attempts"`
	EmbeddingTokens                 int64          `json:"reported_query_embedding_tokens"`
	UnknownEmbeddingUsage           int            `json:"unknown_query_embedding_usage"`
	UnobservedRetrievals            int            `json:"unobserved_retrievals"`
	TokensPerCorrectTask            *float64       `json:"model_tokens_per_correct_task"`
	P50MS                           *int64         `json:"p50_ms"`
	P95MS                           *int64         `json:"p95_ms"`
	LatencySamples                  int            `json:"latency_samples"`
	CorrectDeliverySamples          int            `json:"correct_delivery_samples"`
	CorrectDeliveryP50MS            *int64         `json:"correct_delivery_p50_ms"`
	CorrectDeliveryP95MS            *int64         `json:"correct_delivery_p95_ms"`
	HumanInterventions              int            `json:"human_interventions"`
	TrialsByInterventionLevel       map[string]int `json:"trials_by_intervention_level"`
	CorrectTasksByInterventionLevel map[string]int `json:"correct_tasks_by_intervention_level"`
	InterventionEventsByLevel       map[string]int `json:"intervention_events_by_level"`
	IndexEmbeddingTokens            int64          `json:"reported_index_embedding_tokens"`
	IndexEmbeddingAttempts          int            `json:"index_embedding_attempts"`
	UnknownIndexUsage               int            `json:"unknown_index_embedding_usage"`
	PendingIndexRuns                int            `json:"pending_index_runs"`
	MissingAccountingRuns           int            `json:"missing_accounting_runs"`
	MissingRequestAccountingRuns    int            `json:"missing_request_accounting_runs"`
	KnownHTTPRequests               int64          `json:"known_http_requests"`
	KnownHTTPBytes                  int64          `json:"known_http_bytes"`
	UnknownHTTPObservations         int            `json:"unknown_http_observations"`
}

func Summarize(rows []Trial) []Summary {
	groups := map[string][]Trial{}
	latest := map[string]Trial{}
	for _, r := range rows {
		key := fmt.Sprintf("%s/%s/%s/%s/%d", r.Mode, r.Variant, r.EnvironmentID, r.CaseID, r.Repetition)
		latest[key] = r
	}
	for _, r := range latest {
		key := r.Mode + "/" + r.Variant + "/" + r.EnvironmentID
		groups[key] = append(groups[key], r)
	}
	keys := []string{}
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []Summary{}
	for _, key := range keys {
		trials := groups[key]
		s := Summary{Mode: trials[0].Mode, Variant: trials[0].Variant, EnvironmentID: trials[0].EnvironmentID, Trials: len(trials), TrialsByInterventionLevel: map[string]int{}, CorrectTasksByInterventionLevel: map[string]int{}, InterventionEventsByLevel: map[string]int{}}
		latencies := []int64{}
		correctLatencies := []int64{}
		for _, r := range trials {
			if r.RequestID != "" && r.InformationAccounting == nil {
				s.MissingRequestAccountingRuns++
			}
			if r.InformationAccounting != nil {
				r.Accounting = r.InformationAccounting.RunAccounting
			}
			assessment := aiapp.AssessInterventions(r.Accounting)
			s.TrialsByInterventionLevel[assessment.Level]++
			for level, count := range assessment.EventsByLevel {
				s.InterventionEventsByLevel[level] += count
			}
			if r.Status == "completed" && r.Correct != nil && *r.Correct {
				s.CorrectTasksByInterventionLevel[assessment.Level]++
			}
			if r.FieldScored {
				s.FieldScoredTrials++
				s.MatchedFields += r.MatchedFields
				s.RequiredFields += r.RequiredFields
				s.DeliveredFields += r.DeliveredFields
			}
			if r.Correct != nil {
				s.ScoredTrials++
				if *r.Correct {
					s.CorrectVerdicts++
					if r.Status == "completed" {
						s.CorrectTasks++
					}
				}
			}
			if r.FalseCompletion {
				s.FalseCompletions++
			}
			if r.Accounting != nil && r.Accounting.Interventions != nil {
				s.HumanInterventions += len(r.Accounting.Interventions)
			} else {
				s.HumanInterventions += r.HumanInterventions // Legacy count remains unclassified.
			}
			if r.Accounting == nil {
				s.MissingAccountingRuns++
			} else {
				if !r.Accounting.IndexSettled {
					s.PendingIndexRuns++
				}
				for _, attempt := range r.Accounting.IndexAttempts {
					var metrics knowledge.IndexMetrics
					if json.Unmarshal([]byte(attempt.MetricsJSON), &metrics) != nil {
						s.UnknownIndexUsage++
						continue
					}
					s.IndexEmbeddingAttempts += metrics.EmbeddingCalls
					if metrics.EmbeddingCalls > 0 {
						if metrics.Usage == nil {
							s.UnknownIndexUsage++
						} else {
							s.IndexEmbeddingTokens += metrics.Usage.TotalTokens
						}
					}
				}
			}
			if r.DurationMS > 0 {
				latencies = append(latencies, r.DurationMS)
				if r.Status == "completed" && r.Correct != nil && *r.Correct {
					correctLatencies = append(correctLatencies, r.DurationMS)
				}
			}
			for _, op := range r.Operations {
				if requests, bytes, known, relevant := operationHTTP(op); relevant {
					if known {
						s.KnownHTTPRequests += int64(requests)
						s.KnownHTTPBytes += bytes
					} else {
						s.UnknownHTTPObservations++
					}
				}
				if op.Kind == "search_knowledge" {
					var result aiapp.ToolResult
					var set knowledge.EvidenceSet
					if json.Unmarshal([]byte(op.ResultJSON), &result) != nil || json.Unmarshal(result.Data, &set) != nil || set.Metrics.EmbeddingCalls == 0 {
						s.UnobservedRetrievals++
					} else {
						s.EmbeddingAttempts += set.Metrics.EmbeddingCalls
						if set.Metrics.Usage == nil {
							s.UnknownEmbeddingUsage++
						} else {
							s.EmbeddingTokens += set.Metrics.Usage.TotalTokens
						}
					}
				}
			}
			s.SearchAttempts += searchAttempts(r)
			for _, call := range modelCalls(r) {
				search := call.Purpose == "search_api" || call.Purpose == "discover_sources"
				if search {
					s.SearchModelAttempts++
				} else {
					s.ModelAttempts++
				}
				if call.Usage == nil {
					if search {
						s.UnknownSearchModelUsage++
					} else {
						s.UnknownUsageAttempts++
					}
					continue
				}
				if search {
					s.SearchModelTokens += call.Usage.TotalTokens
				} else {
					s.ReportedTokens += call.Usage.TotalTokens
				}
				s.LLMCachedPromptTokens += call.Usage.Cached()
				if d := call.Usage.CompletionTokensDetail; d != nil && d.ReasoningTokens != nil {
					s.ReasoningTokens += *d.ReasoningTokens
				} else {
					s.MissingReasoningDetails++
				}
			}
			if r.InformationAccounting != nil {
				for _, retrieval := range r.InformationAccounting.Retrievals {
					if retrieval.Metrics == nil {
						s.UnobservedRetrievals++
						continue
					}
					s.EmbeddingAttempts += retrieval.Metrics.EmbeddingCalls
					if retrieval.Metrics.EmbeddingCalls > 0 {
						if retrieval.Metrics.Usage == nil {
							s.UnknownEmbeddingUsage++
						} else {
							s.EmbeddingTokens += retrieval.Metrics.Usage.TotalTokens
						}
					}
				}
			}
		}
		s.LLMReportedTokens = s.ReportedTokens + s.SearchModelTokens
		if s.CorrectTasks > 0 && s.UnknownUsageAttempts == 0 && s.UnknownSearchModelUsage == 0 && s.MissingRequestAccountingRuns == 0 && s.ScoredTrials == s.Trials {
			value := float64(s.LLMReportedTokens) / float64(s.CorrectTasks)
			s.TokensPerCorrectTask = &value
		}
		s.LatencySamples = len(latencies)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		if len(latencies) > 0 {
			x := latencies[int(math.Ceil(float64(len(latencies))*.5))-1]
			s.P50MS = &x
		}
		if len(latencies) >= 20 {
			x := latencies[int(math.Ceil(float64(len(latencies))*.95))-1]
			s.P95MS = &x
		}
		s.CorrectDeliverySamples = len(correctLatencies)
		sort.Slice(correctLatencies, func(i, j int) bool { return correctLatencies[i] < correctLatencies[j] })
		if len(correctLatencies) > 0 {
			x := correctLatencies[int(math.Ceil(float64(len(correctLatencies))*.5))-1]
			s.CorrectDeliveryP50MS = &x
		}
		if len(correctLatencies) >= 20 {
			x := correctLatencies[int(math.Ceil(float64(len(correctLatencies))*.95))-1]
			s.CorrectDeliveryP95MS = &x
		}
		if s.FieldScoredTrials > 0 {
			s.FieldCoverage = fieldRatio(s.MatchedFields, s.RequiredFields)
			s.FieldPrecision = fieldRatio(s.MatchedFields, s.DeliveredFields)
		}
		out = append(out, s)
	}
	return out
}

func modelCalls(row Trial) []aiapp.ModelCallAccounting {
	if row.InformationAccounting != nil {
		return row.InformationAccounting.ModelCalls
	}
	return aiapp.CrawlerModelCalls(row.Operations)
}

func searchAttempts(row Trial) int {
	if row.InformationAccounting != nil {
		return len(row.InformationAccounting.SearchCalls)
	}
	n := 0
	for _, op := range row.Operations {
		if op.Kind == "discover_sources" {
			n++
		}
	}
	return n
}
