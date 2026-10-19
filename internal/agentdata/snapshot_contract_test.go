package agentdata

import (
	"encoding/json"
	"testing"
	"time"
)

func testSnapshotScope() SnapshotScope {
	return SnapshotScope{SourceID: "catalog", Endpoint: "https://example.com/items?region=cn", Params: map[string]any{"mode": "all"}, ContractVersion: "v2", ContractHash: "output-contract"}
}
func testSnapshotPage(id, cursor, body string) SnapshotPage {
	u := "https://example.com/items?region=cn"
	if cursor != "" {
		u += "&cursor=" + cursor
	}
	return SnapshotPage{ReceiptID: id, RequestedURL: u, Method: "GET", Status: 200, FetchedAt: time.Now().UTC(), Body: json.RawMessage(body)}
}
func testPaginationProtocol() PaginationProtocol {
	scopeID, _ := testSnapshotScope().Key()
	return PaginationProtocol{ScopeID: scopeID, Version: "json-cursor-v1", CursorParameter: "cursor", ItemsPointer: "/items", NextPointer: "/next", TotalPointer: "/total"}
}

func TestSnapshotContractApprovedProtocolCannotMoveToAnotherScope(t *testing.T) {
	approved := testPaginationProtocol()
	for _, dimension := range []string{"contract", "region", "unchanged"} {
		t.Run(dimension, func(t *testing.T) {
			scope := testSnapshotScope()
			page := testSnapshotPage("p1", "", `{"items":[{"id":"a"}],"next":null,"total":1}`)
			if dimension == "contract" {
				scope.ContractHash = "different-contract"
			}
			if dimension == "region" {
				scope.Endpoint = "https://example.com/items?region=us"
				page.RequestedURL = scope.Endpoint
			}
			got, err := AssessSnapshot(scope, 1, SnapshotProposal{Kind: SnapshotFull, ReceiptIDs: []string{"p1"}}, &approved, []SnapshotPage{page}, []SnapshotItemBinding{{ReceiptID: "p1", ItemPointer: "/items/0", RecordKey: "a", Supported: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.CanRetireMissing() != (dimension == "unchanged") {
				t.Fatal("pagination approval crossed its frozen scope", got)
			}
		})
	}
}

func TestSnapshotContractSourceExhaustionIsNotDeliveredInventory(t *testing.T) {
	scope := testSnapshotScope()
	protocol := testPaginationProtocol()
	pages := []SnapshotPage{testSnapshotPage("p1", "", `{"items":[{"id":"a"},{"id":"b"}],"next":null,"total":2}`)}
	proposal := SnapshotProposal{Kind: SnapshotFull, ReceiptIDs: []string{"p1"}}
	rows := []SnapshotItemBinding{{ReceiptID: "p1", ItemPointer: "/items/0", RecordKey: "a", Supported: true}}
	got, err := AssessSnapshot(scope, 1, proposal, &protocol, pages, rows)
	if err != nil || !got.SourceComplete || got.RecordsComplete || got.Kind != SnapshotPartial || got.CanRetireMissing() {
		t.Fatal("last page incorrectly authorized missing-record deletion", got, err)
	}
	rows = append(rows, SnapshotItemBinding{ReceiptID: "p1", ItemPointer: "/items/1", RecordKey: "b", Supported: true})
	got, err = AssessSnapshot(scope, 1, proposal, &protocol, pages, rows)
	if err != nil || got.Kind != SnapshotFull || !got.CanRetireMissing() {
		t.Fatal("complete verified inventory rejected", got, err)
	}
	rows[1].Supported = false
	got, err = AssessSnapshot(scope, 1, proposal, &protocol, pages, rows)
	if err != nil || got.CanRetireMissing() {
		t.Fatal("unsupported source item disappeared from the denominator", got, err)
	}
}

func TestSnapshotContractPaginationAndScopeCannotBeClaimedByAI(t *testing.T) {
	scope := testSnapshotScope()
	protocol := testPaginationProtocol()
	p1 := testSnapshotPage("p1", "", `{"items":[{"id":"a"}],"next":"second","total":2}`)
	p2 := testSnapshotPage("p2", "second", `{"items":[{"id":"b"}],"next":null,"total":2}`)
	proposal := SnapshotProposal{Kind: SnapshotFull, ReceiptIDs: []string{"p1", "p2"}}
	rows := []SnapshotItemBinding{{ReceiptID: "p1", ItemPointer: "/items/0", RecordKey: "a", Supported: true}, {ReceiptID: "p2", ItemPointer: "/items/0", RecordKey: "b", Supported: true}}
	got, err := AssessSnapshot(scope, 2, proposal, &protocol, []SnapshotPage{p1, p2}, rows)
	if err != nil || !got.CanRetireMissing() {
		t.Fatal("complete pagination rejected", got, err)
	}
	for _, mode := range []string{"missing-page", "foreign-scope", "missing-next", "wrong-total", "foreign-item", "untrusted-protocol"} {
		pages := []SnapshotPage{p1, p2}
		bindings := append([]SnapshotItemBinding{}, rows...)
		supportedProtocol := &protocol
		switch mode {
		case "missing-page":
			pages = pages[:1]
		case "foreign-scope":
			pages[1].RequestedURL = "https://example.com/items?region=us&cursor=second"
		case "missing-next":
			pages[1].Body = json.RawMessage(`{"items":[{"id":"b"}],"total":2}`)
		case "wrong-total":
			pages[1].Body = json.RawMessage(`{"items":[{"id":"b"}],"next":null,"total":3}`)
		case "foreign-item":
			bindings[1].ReceiptID = "made-up"
		case "untrusted-protocol":
			supportedProtocol = nil
		}
		got, err := AssessSnapshot(scope, 2, proposal, supportedProtocol, pages, bindings)
		if err != nil || got.Kind != SnapshotPartial || got.CanRetireMissing() {
			t.Fatal("unsafe completeness accepted", mode, got, err)
		}
	}
}

func TestSnapshotContractIdentityAndExplicitIncrementalStayDistinct(t *testing.T) {
	scope := testSnapshotScope()
	key, err := scope.Key()
	if err != nil {
		t.Fatal(err)
	}
	same := scope
	same.Params = map[string]any{"mode": "all"}
	same.Endpoint = "https://EXAMPLE.com/items?region=cn"
	if other, err := same.Key(); err != nil || other != key {
		t.Fatal("equivalent scope changed identity", err)
	}
	same.Endpoint = "https://example.com/items?region=us"
	if other, err := same.Key(); err != nil || other == key {
		t.Fatal("different collection range collapsed", err)
	}
	got, err := AssessSnapshot(scope, 3, SnapshotProposal{Kind: SnapshotIncremental}, nil, nil, nil)
	if err != nil || got.ProposedKind != SnapshotIncremental || got.Kind != SnapshotPartial || got.CanRetireMissing() {
		t.Fatal("unverified delta became a full snapshot", got, err)
	}
	if _, err := AssessSnapshot(scope, 0, SnapshotProposal{Kind: SnapshotPartial}, nil, nil, nil); err == nil {
		t.Fatal("model/zero generation admitted")
	}
}
