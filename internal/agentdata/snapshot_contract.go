package agentdata

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type SnapshotKind string

const (
	SnapshotFull        SnapshotKind = "full_snapshot"
	SnapshotPartial     SnapshotKind = "partial"
	SnapshotIncremental SnapshotKind = "explicit_incremental"
)

// Source identity and range are bound by Go; generation is a separate ordinal,
// not an adapter/content hash. Parameters must already pass their typed schema.
type SnapshotScope struct {
	SourceID        string         `json:"source_id"`
	Endpoint        string         `json:"endpoint"`
	Params          map[string]any `json:"params"`
	ContractVersion string         `json:"contract_version"`
	ContractHash    string         `json:"contract_hash"`
}

func (s SnapshotScope) Key() (string, error) {
	if s.SourceID == "" || s.ContractVersion == "" || s.ContractHash == "" {
		return "", fmt.Errorf("incomplete host collection scope")
	}
	endpoint, err := CanonicalEndpoint(s.Endpoint)
	if err != nil {
		return "", err
	}
	params, err := CanonicalParameters(s.Params)
	if err != nil {
		return "", err
	}
	return Hash([]any{"scope-v1", s.SourceID, endpoint, params, s.ContractVersion, s.ContractHash}), nil
}

// The proposal has no trusted completeness flag, generation, or scope ID.
type SnapshotProposal struct {
	Kind       SnapshotKind `json:"kind"`
	ReceiptIDs []string     `json:"receipt_ids,omitempty"`
}
type SnapshotManifest struct {
	ScopeID         string       `json:"scope_id"`
	Generation      uint64       `json:"generation"`
	ProposedKind    SnapshotKind `json:"proposed_kind"`
	Kind            SnapshotKind `json:"kind"`
	SourceComplete  bool         `json:"source_complete"`
	RecordsComplete bool         `json:"records_complete"`
	CheckerVersion  string       `json:"checker_version,omitempty"`
	ReceiptIDs      []string     `json:"receipt_ids,omitempty"`
	Issues          []string     `json:"issues,omitempty"`
}

func (s SnapshotManifest) CanRetireMissing() bool {
	return s.Kind == SnapshotFull && s.ScopeID != "" && s.Generation > 0 && s.SourceComplete && s.RecordsComplete && s.CheckerVersion != "" && len(s.ReceiptIDs) > 0
}

// These two inputs come from the trusted host, never from a Lua return value:
// persisted actual responses, and item-to-record bindings checked in 0.16.
type SnapshotPage struct {
	ReceiptID    string
	RequestedURL string
	Method       string
	Status       int
	FetchedAt    time.Time
	Body         json.RawMessage
}
type SnapshotItemBinding struct {
	ReceiptID, ItemPointer, RecordKey string
	Supported                         bool
}

// Only a host-approved, immutable pagination mapping may be supplied here.
// The Agent's suggested pointer is not a verified pagination protocol.
type PaginationProtocol struct {
	ScopeID         string
	Version         string
	CursorParameter string
	ItemsPointer    string
	NextPointer     string
	TotalPointer    string
}

func AssessSnapshot(scope SnapshotScope, generation uint64, proposal SnapshotProposal, protocol *PaginationProtocol, pages []SnapshotPage, bindings []SnapshotItemBinding) (SnapshotManifest, error) {
	key, err := scope.Key()
	if err != nil {
		return SnapshotManifest{}, err
	}
	if generation == 0 {
		return SnapshotManifest{}, fmt.Errorf("host collection generation is required")
	}
	switch proposal.Kind {
	case SnapshotFull, SnapshotPartial, SnapshotIncremental:
	default:
		return SnapshotManifest{}, fmt.Errorf("unsupported collection proposal")
	}
	out := SnapshotManifest{ScopeID: key, Generation: generation, ProposedKind: proposal.Kind, Kind: SnapshotPartial}
	incomplete := func(reason string) (SnapshotManifest, error) {
		out.Issues = append(out.Issues, reason)
		return out, nil
	}
	if proposal.Kind != SnapshotFull {
		return incomplete("no verified full-snapshot claim; absence cannot mean deletion")
	}
	if protocol == nil || protocol.ScopeID != key || protocol.Version != "json-cursor-v1" || protocol.CursorParameter == "" || protocol.ItemsPointer == "" || protocol.NextPointer == "" {
		return incomplete("no host-approved supported pagination protocol")
	}
	if len(proposal.ReceiptIDs) == 0 {
		return incomplete("no persisted receipt references")
	}
	ledger := map[string]SnapshotPage{}
	for _, page := range pages {
		if page.ReceiptID == "" {
			return incomplete("persisted receipt identity missing")
		}
		if _, exists := ledger[page.ReceiptID]; exists {
			return incomplete("ambiguous receipt identity")
		}
		ledger[page.ReceiptID] = page
	}
	base, err := CanonicalEndpoint(scope.Endpoint)
	if err != nil {
		return out, err
	}
	baseURL, _ := url.Parse(base)
	if _, exists := baseURL.Query()[protocol.CursorParameter]; exists {
		return incomplete("scope does not begin at the initial pagination boundary")
	}
	expectedCursor := ""
	seenCursor := map[string]bool{}
	items := map[string]bool{}
	total := int64(-1)
	for ordinal, id := range proposal.ReceiptIDs {
		page, exists := ledger[id]
		if !exists || page.Method != "GET" || page.Status < 200 || page.Status >= 300 || page.FetchedAt.IsZero() {
			return incomplete("referenced successful response is unavailable")
		}
		actual, err := url.Parse(page.RequestedURL)
		if err != nil {
			return incomplete("invalid persisted request URL")
		}
		query, err := url.ParseQuery(actual.RawQuery)
		if err != nil {
			return incomplete("invalid persisted query")
		}
		cursorValues := query[protocol.CursorParameter]
		if len(cursorValues) > 1 || query.Get(protocol.CursorParameter) != expectedCursor {
			return incomplete("pagination request does not follow the observed cursor")
		}
		delete(query, protocol.CursorParameter)
		actual.RawQuery = query.Encode()
		actualBase, e := CanonicalEndpoint(actual.String())
		if e != nil || actualBase != base {
			return incomplete("page belongs to a different endpoint or collection range")
		}
		if seenCursor[expectedCursor] {
			return incomplete("pagination cursor repeated")
		}
		seenCursor[expectedCursor] = true
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(page.Body)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil || !json.Valid(page.Body) {
			return incomplete("persisted JSON response is unreadable")
		}
		array, ok := snapshotPointer(value, protocol.ItemsPointer)
		if !ok {
			return incomplete("item collection locator missing")
		}
		rows, ok := array.([]any)
		if !ok {
			return incomplete("item locator is not an array")
		}
		for index := range rows {
			items[id+"\x00"+protocol.ItemsPointer+"/"+strconv.Itoa(index)] = true
		}
		if protocol.TotalPointer != "" {
			declared, ok := snapshotPointer(value, protocol.TotalPointer)
			if !ok {
				return incomplete("declared total locator missing")
			}
			number, ok := declared.(json.Number)
			if !ok {
				return incomplete("declared total is not an integer")
			}
			n, err := number.Int64()
			if err != nil || n < 0 || (total >= 0 && total != n) {
				return incomplete("inconsistent declared total")
			}
			total = n
		}
		next, ok := snapshotPointer(value, protocol.NextPointer)
		if !ok {
			return incomplete("missing cursor is not a proved terminal cursor")
		}
		nextCursor := ""
		if next != nil {
			var valid bool
			nextCursor, valid = next.(string)
			if !valid {
				return incomplete("unsupported next-cursor type")
			}
		}
		terminal := nextCursor == ""
		if terminal != (ordinal == len(proposal.ReceiptIDs)-1) {
			return incomplete("incomplete or surplus pagination chain")
		}
		expectedCursor = nextCursor
	}
	if total >= 0 && total != int64(len(items)) {
		return incomplete("observed source inventory differs from declared total")
	}
	out.SourceComplete = true
	out.ReceiptIDs = append([]string{}, proposal.ReceiptIDs...)
	out.CheckerVersion = "scope-json-cursor-v1"
	represented := map[string]string{}
	for _, binding := range bindings {
		ref := binding.ReceiptID + "\x00" + binding.ItemPointer
		if !binding.Supported || binding.RecordKey == "" || !items[ref] {
			return incomplete("unverified or foreign item-to-record binding")
		}
		if prior, exists := represented[ref]; exists && prior != binding.RecordKey {
			return incomplete("one source item maps to conflicting records")
		}
		represented[ref] = binding.RecordKey
	}
	if len(represented) != len(items) {
		return incomplete("not every source item has a supported delivered record")
	}
	out.RecordsComplete = true
	out.Kind = SnapshotFull
	return out, nil
}

func snapshotPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		// Invalid RFC 6901 escapes never resolve to an accidental field.
		for i := 0; i < len(part); i++ {
			if part[i] == '~' {
				if i+1 >= len(part) || (part[i+1] != '0' && part[i+1] != '1') {
					return nil, false
				}
				i++
			}
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var exists bool
			value, exists = v[part]
			if !exists {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(v) || strconv.Itoa(index) != part {
				return nil, false
			}
			value = v[index]
		default:
			return nil, false
		}
	}
	return value, true
}
