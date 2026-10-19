package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

// Continuing a read must advance. Before the fix, continue_from resumed at the
// read's starting offset instead of where it stopped, so a caller following the
// host's own next_read guidance received the same opening slice every time. In
// the recorded run that produced six identical reads of the same 1200
// characters, each one paid for with a model call.
func TestContinueFromAdvancesPastThePreviousRead(t *testing.T) {
	body := `<main><p>` + strings.Repeat("西南交通大学采购公告正文段落。", 900) + `</p></main>`
	s, run, _ := storedHTMLReader(t, body)
	ctx := context.Background()

	first, err := s.readData(ctx, run, ToolCall{ID: "read-1", Function: FunctionCall{
		Name:      "read_data",
		Arguments: `{"handle":"source","view":"content","limit":1200}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var firstData map[string]any
	if err := json.Unmarshal(first.Data, &firstData); err != nil {
		t.Fatal(err)
	}
	firstText, _ := firstData["text"].(string)
	if len([]rune(firstText)) == 0 {
		t.Fatalf("first read returned no text: %s", first.Data)
	}
	if firstData["next_read"] == nil {
		t.Fatalf("first read offered no continuation: %s", first.Data)
	}

	// Persist the first read the way the loop does, so continue_from can find it.
	raw, _ := json.Marshal(first)
	if err := s.Data.DB.Create(&agentdata.Operation{
		RunID: run.ID, Key: "tool:read-1", Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw),
	}).Error; err != nil {
		t.Fatal(err)
	}

	second, err := s.readData(ctx, run, ToolCall{ID: "read-2", Function: FunctionCall{
		Name:      "read_data",
		Arguments: `{"continue_from":"read-1"}`,
	}})
	if err != nil {
		t.Fatalf("continuation failed: %v", err)
	}
	var secondData map[string]any
	if err := json.Unmarshal(second.Data, &secondData); err != nil {
		t.Fatal(err)
	}
	secondText, _ := secondData["text"].(string)

	if secondText == firstText {
		t.Fatalf("continuation returned the same slice as the previous read (%d runes); the caller never advances", len([]rune(secondText)))
	}
	if len([]rune(secondText)) == 0 {
		t.Fatalf("continuation returned no text: %s", second.Data)
	}
	// The resumed offset must be past the first slice, not back at zero.
	if off, ok := secondData["offset"].(float64); ok && off == 0 {
		t.Fatalf("continuation restarted at offset 0 instead of advancing")
	}
}
