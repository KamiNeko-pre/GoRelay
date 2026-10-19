package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestBusinessIdentityCannotBeRenamedThroughSourceHandles(t *testing.T) {
	s, r, raw := storedHTMLReader(t, `<article id="news"><h2>Observed title</h2><p>`+strings.Repeat("source material ", 100)+`</p></article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var original ToolResult
	json.Unmarshal([]byte(raw), &original)
	// A host-restored receipt has a new protocol ID, but owns the same source.
	for _, id := range []string{"restored-source", "legacy-same-snapshot"} {
		copy := original
		copy.CallID, copy.Handle = id, id
		if id == "restored-source" {
			copy.ReplayOf = "tool:source"
		} else {
			var page sourceInspection
			json.Unmarshal(copy.Data, &page)
			page.Source.Receipt.FetchedAt = page.Source.Receipt.FetchedAt.Add(time.Hour)
			copy.Data, _ = json.Marshal(page)
		}
		encoded, _ := json.Marshal(copy)
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + id, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(encoded)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	call := ToolCall{ID: "first-region", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"#news"}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"restored-source", "legacy-same-snapshot", first.CallID} {
		call.ID = "renamed-" + handle
		call.Function.Arguments = `{"handle":"` + handle + `","view":"dom","selector":"#news"}`
		out, err := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{first.CallID: true})
		if err == nil || out.Failure == nil || out.Failure.Code != "DUPLICATE_TOOL_CALL" {
			t.Errorf("handle %s bypassed business identity: failure=%+v error=%v", handle, out.Failure, err)
		}
	}
	// Same source, genuinely different region remains useful work.
	call.ID = "new-region"
	call.Function.Arguments = `{"handle":"restored-source","view":"dom","selector":"h2"}`
	if _, err := s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal("distinct observed region was suppressed:", err)
	}
}

func TestBusinessIdentityKeepsDifferentSelectorParameters(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<article id="news" class="entry"><h2>Observed title</h2></article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for i, selector := range []string{"#news", "article", ".entry"} {
		raw, _ := json.Marshal(map[string]any{"handle": "source", "view": "dom", "selector": selector})
		call := ToolCall{ID: fmt.Sprintf("selector-%d", i), Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}}
		out, err := s.executeDurableTool(ctx, r, call)
		if err != nil {
			t.Fatalf("different selector %q was suppressed: %+v %v", selector, out.Failure, err)
		}
		call.ID += "-repeat"
		out, err = s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{out.CallID: true})
		if err == nil || out.Failure == nil || out.Failure.Code != "DUPLICATE_TOOL_CALL" {
			t.Fatalf("same selector %q ran again: %+v %v", selector, out.Failure, err)
		}
	}
}

func TestBusinessIdentityUnifiesCursorAndDirectContinuation(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<article>`+strings.Repeat(`<p><a href="/detail">source link</a></p>`, 30)+`</article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	args := dataReadArgs{Handle: "source", View: "dom", Selector: "p", Limit: 3}
	raw, _ := json.Marshal(args)
	call := ToolCall{ID: "page-1", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Next string `json:"next_cursor"`
	}
	json.Unmarshal(first.Data, &page)
	if page.Next == "" {
		t.Fatal("no continuation")
	}
	call.ID = "cursor-page"
	call.Function.Arguments = `{"cursor":"` + page.Next + `"}`
	second, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	call.ID = "continue-alias"
	call.Function.Arguments = `{"continue_from":"page-1"}`
	out, err := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{second.CallID: true})
	if err == nil || out.Failure == nil || out.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Errorf("continue_from bypassed opaque cursor identity: %+v %v", out.Failure, err)
	}
	direct, err := cursorRoundTrip(page.Next)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(direct)
	call.ID = "direct-page"
	call.Function.Arguments = string(raw)
	out, err = s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{second.CallID: true})
	if err == nil || out.Failure == nil || out.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Errorf("direct offset bypassed cursor identity: %+v %v", out.Failure, err)
	}
}

func TestBusinessIdentityCannotReadProseViaReceiptAliases(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<article><p>`+strings.Repeat("observed source text ", 100)+`</p></article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "source-region", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"article","limit":64}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	call.ID = "receipt-text-alias"
	call.Function.Arguments = `{"handle":"` + first.CallID + `","view":"json","pointer":"/text","limit":64}`
	out, err := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{first.CallID: true})
	if err == nil || out.Failure == nil || out.Failure.Code != "CRAWLER_STRUCTURE_ONLY" {
		t.Fatalf("own text receipt became new work: %+v %v", out.Failure, err)
	}
}
