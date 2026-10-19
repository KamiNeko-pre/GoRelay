package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

// A page must be readable in a small, bounded number of calls. Before the read
// floor and the shorter cursor, the observation budget forced the pager to halve
// down to roughly two hundred characters per call, so a ten-thousand-character
// page needed about fifty reads and no progress window could accommodate it.
//
// This walks the page exactly as the loop does: read, persist the receipt, then
// continue from the host's own next_read reference.
func TestLongPageIsReadableInBoundedCalls(t *testing.T) {
	// Chinese text is the worst case: three UTF-8 bytes per rune against a
	// byte-denominated observation budget.
	body := `<main><p>` + strings.Repeat("西南交通大学采购公告正文内容。", 400) + `</p></main>`
	s, run, _ := storedHTMLReader(t, body)
	ctx := context.Background()

	readOnce := func(callID, args string) map[string]any {
		out, err := s.readData(ctx, run, ToolCall{ID: callID, Function: FunctionCall{Name: "read_data", Arguments: args}})
		if err != nil {
			t.Fatalf("read %s failed: %v", callID, err)
		}
		raw, _ := json.Marshal(out)
		if err := s.Data.DB.Create(&agentdata.Operation{
			RunID: run.ID, Key: "tool:" + callID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw),
		}).Error; err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(out.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}

	calls := 0
	seen := 0
	total := 0
	args := `{"handle":"source","view":"content","limit":1200}`
	for calls = 0; calls < 30; calls++ {
		data := readOnce(fmt.Sprintf("read-%d", calls), args)
		if text, ok := data["text"].(string); ok {
			seen += len([]rune(text))
		}
		if tr, ok := data["total_runes"].(float64); ok {
			total = int(tr)
		}
		next, ok := data["next_read"].(map[string]any)
		if !ok {
			break
		}
		from, _ := next["continue_from"].(string)
		if from == "" {
			break
		}
		raw, _ := json.Marshal(map[string]any{"continue_from": from})
		args = string(raw)
	}

	if total == 0 {
		t.Fatal("page total never reported")
	}
	if calls > 12 {
		t.Fatalf("page of %d runes still needs %d reads; a bounded window cannot accommodate this", total, calls)
	}
	if seen < total/2 {
		t.Fatalf("only %d of %d runes were reachable in %d reads", seen, total, calls)
	}
	t.Logf("read %d of %d runes in %d calls", seen, total, calls)
}

// The continuation a model must echo back has to be small, because whatever it
// carries is paid for out of the same observation budget as the text it
// continues. The reader now prefers continue_from (one call id) over the base64
// cursor, and the cursor itself no longer carries the run, receipt or version.
func TestContinuationReferenceStaysSmall(t *testing.T) {
	short := map[string]any{"continue_from": "call_00_aVeryLongOpaqueHandleIdentifier1234"}
	raw, _ := json.Marshal(short)
	if len(raw) > 80 {
		t.Fatalf("continue_from reference is %d bytes", len(raw))
	}

	args := dataReadArgs{
		Handle:      "call_00_aVeryLongOpaqueHandleIdentifierValue1234567890",
		RunID:       "8c3805b72a107d39a2a73a61ed90aa06",
		ReceiptID:   45,
		View:        "content",
		ViewVersion: htmlInformationVersion,
		Pointer:     "/data/source/body",
	}
	cursor := nextReadCursor(args, 2400)
	if len(cursor) > 260 {
		t.Fatalf("fallback cursor is %d bytes; it competes with the text it continues", len(cursor))
	}
	var back dataReadArgs
	decoded, err := cursorRoundTrip(cursor)
	if err != nil {
		t.Fatal(err)
	}
	back = decoded
	if back.Offset != 2400 {
		t.Fatalf("cursor lost its offset: %+v", back)
	}
	// The continuation must carry the projection identity, or the reader refuses
	// the offset as unversioned and the caller restarts from zero.
	if back.ViewVersion == "" {
		t.Fatalf("cursor dropped the view version: %+v", back)
	}
}
