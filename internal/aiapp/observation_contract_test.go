package aiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestObservationSchemaDeclaresRuntimePageLimits(t *testing.T) {
	for _, def := range AgentToolDefinitions() {
		if def.Function.Name != "read_observation" {
			continue
		}
		var schema struct{ Properties map[string]map[string]any }
		if err := json.Unmarshal(def.Function.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Properties["limit"]["maximum"] != float64(2000) || schema.Properties["limit"]["minimum"] != float64(0) || schema.Properties["offset"]["minimum"] != float64(0) {
			t.Fatalf("advertised arguments omit actual limits: %s", def.Function.Parameters)
		}
		return
	}
	t.Fatal("missing observation tool")
}

func TestObservationPreservesReferencesAndExportsExactReadArguments(t *testing.T) {
	for _, collection := range []string{"entries", "records"} {
		id := "ev_" + strings.Repeat("a", 80)
		url := "https://example.org/report?" + strings.Repeat("query=long&", 80)
		rows := []any{}
		for n := 0; n < 12; n++ {
			rows = append(rows, map[string]any{"id": id, "url": url, "revision": "revision-complete", "text": strings.Repeat("资料正文", 5000)})
		}
		raw, _ := json.Marshal(map[string]any{collection: rows})
		got := boundedObservation(ToolResult{CallID: "original", Name: "search_knowledge", Data: raw})
		if len(got.Data) > observationBytes {
			t.Fatalf("observation exceeds budget: %d", len(got.Data))
		}
		encodedURL, _ := json.Marshal(url)
		if !strings.Contains(string(got.Data), id) || !strings.Contains(string(got.Data), string(encodedURL)) {
			t.Fatalf("opaque identity was clipped: %s", got.Data)
		}
		var view map[string]any
		if err := json.Unmarshal(got.Data, &view); err != nil {
			t.Fatal(err)
		}
		reads, ok := view["reads"].([]any)
		if !ok || len(reads) == 0 {
			t.Fatalf("missing ready-to-use read arguments: %s", got.Data)
		}
		foundText, foundPage := false, false
		for _, item := range reads {
			read := item.(map[string]any)
			args := read["arguments"].(map[string]any)
			if args["tool_call_id"] != "original" {
				t.Fatal("reader points to projected rather than original receipt")
			}
			pointer := args["pointer"].(string)
			if strings.HasSuffix(pointer, "/text") {
				foundText = true
			}
			if pointer == "/data/"+collection {
				foundPage = true
			}
		}
		if !foundText || !foundPage {
			t.Fatalf("missing text/collection reader: %s", got.Data)
		}
	}
	// A small scalar receipt needs neither shape changes nor recovery calls.
	unchanged := ToolResult{Data: json.RawMessage(`{"id":"ev_small","status":"ready"}`)}
	if string(boundedObservation(unchanged).Data) != string(unchanged.Data) {
		t.Fatal("small valid receipt changed")
	}
}

func TestContextReductionKeepsHandlesInsideOriginalReceipt(t *testing.T) {
	entries := []any{}
	for n := 0; n < 8; n++ {
		entries = append(entries, map[string]any{"id": "ev_complete", "text": strings.Repeat("证据", 5000)})
	}
	original := map[string]any{"entries": entries}
	raw, _ := json.Marshal(original)
	first := boundedObservation(ToolResult{CallID: "original", Name: "search_knowledge", Data: raw})
	second := observationWithLimit(first, 2048)
	var view struct {
		Reads []observationRead `json:"reads"`
	}
	if err := json.Unmarshal(second.Data, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Reads) == 0 {
		t.Fatal("missing original reader")
	}
	for _, read := range view.Reads {
		pointer := read.Arguments["pointer"].(string)
		if _, err := jsonPointer(map[string]any{"data": original}, pointer); err != nil {
			t.Fatalf("projection invented a path absent from original receipt: %s", pointer)
		}
	}
}
