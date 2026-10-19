package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/agentdata"
)

// domRowNodes projects a requested page of matched nodes. New crawler runs
// budget the complete structural page at the whole-context boundary.
//
// Each row reports its own text and attributes, and — when the row element
// itself carries no href — the anchors it contains, resolved against the fetched
// URL. A listing row is normally `<li><a href="info/1016/75197.htm">title</a></li>`,
// so the row element has no target of its own: a crawler reading only
// attributes holds every title and cannot address one detail page. A measured
// real run did exactly that, matched 15 rows, received `"attributes": {}` for
// all of them, and re-read the same selector until its budget ran out.
//
// Resolving here rather than leaving it to the caller is deliberate: the host
// already knows the fetched URL, and a relative href is not a request target.
// Reporting the anchors a row really holds stays page-shape independent.
func domRowNodes(sourceURL string, selection *goquery.Selection, offset, limit int, byteLimits ...int) []map[string]any {
	byteLimit := observationContentBytes
	if len(byteLimits) > 0 {
		byteLimit = byteLimits[0]
	}
	nodes := []map[string]any{}
	selection.EachWithBreak(func(index int, node *goquery.Selection) bool {
		if index < offset {
			return true
		}
		if len(nodes) >= limit || len(node.Nodes) == 0 {
			return false
		}
		text := normalizeEvidenceText(visibleHTMLText(node.Nodes[0]))
		shown := fitObservationText(text, 1024)
		rawAttrs := map[string]string{}
		for _, attr := range node.Nodes[0].Attr {
			if attr.Key != "style" && attr.Key != "srcdoc" && !strings.HasPrefix(attr.Key, "on") {
				rawAttrs[attr.Key] = attr.Val
			}
		}
		attrs := map[string]string{}
		for _, key := range []string{"href", "src", "id", "class", "title", "datetime", "role"} {
			if value, ok := node.Attr(key); ok {
				attrs[key] = value
			}
		}
		// A row that is itself a link keeps its target, made absolute. A row that
		// is not a link has no href of its own and gets one from its anchors below.
		if own := attrs["href"]; own != "" {
			if resolved := resolveAgainst(sourceURL, own); resolved != "" {
				attrs["href"] = resolved
			} else {
				delete(attrs, "href")
			}
		}
		// Raw values are extraction facts; resolved targets are navigation aids.
		// Keep both so models need not guess what Lua/evidence attribute reads mean.
		entry := map[string]any{"index": index, "text": shown, "attributes": attrs, "raw_attributes": rawAttrs, "text_truncated": shown != text}
		if _, hasOwn := attrs["href"]; !hasOwn {
			links := []map[string]string{}
			node.Find("a[href]").EachWithBreak(func(_ int, anchor *goquery.Selection) bool {
				if len(anchor.Nodes) == 0 {
					return true
				}
				href := resolveAgainst(sourceURL, attrOf(anchor, "href"))
				if href == "" {
					return true
				}
				links = append(links, map[string]string{
					"href":     href,
					"raw_href": attrOf(anchor, "href"),
					"text":     fitObservationText(normalizeEvidenceText(visibleHTMLText(anchor.Nodes[0])), 200),
				})
				return len(links) < 8
			})
			if len(links) > 0 {
				entry["links"] = links
			}
		}
		if byteLimit > 0 {
			candidate := append(append([]map[string]any{}, nodes...), entry)
			raw, _ := json.Marshal(candidate)
			if len(raw) > byteLimit {
				return false
			}
		}
		nodes = append(nodes, entry)
		return true
	})
	return nodes
}

func attrOf(sel *goquery.Selection, key string) string {
	value, _ := sel.Attr(key)
	return value
}

// resolveAgainst makes an href usable as a request target.
//
// Rows publish relative targets ("info/1016/75197.htm"), and a crawler that has
// to reconstruct the base from the fetched URL before it can follow a row is
// doing work the host already knows how to do. Only http/https results are
// returned, so a javascript: or mailto: anchor is reported as no target rather
// than as a URL the caller would try to fetch.
func resolveAgainst(sourceURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	base, err := url.Parse(sourceURL)
	if err != nil || base == nil {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(u)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	return resolved.String()
}

// Exact reads address a rebuildable representation of one persisted receipt.
// Large children are references, not repeatedly truncated anonymous blobs.
func (s *Service) readDetailedData(ctx context.Context, run agentdata.Run, call ToolCall, op agentdata.Operation, a dataReadArgs, receipt ToolResult) (ToolResult, error) {
	contextManaged := crawlerRun(run) && promptPolicyForRun(run.BudgetJSON) != nil
	meta := map[string]any{"handle": a.Handle, "receipt_id": op.ID, "new_http_requests": 0}
	version := "receipt-json-v1"
	var value any
	if !isFetchedInspection(receipt) {
		if a.View != "json" || a.Selector != "" || a.BlockID != "" {
			// This handle is a tool receipt, not a fetched page. Only its JSON is
			// readable. Name which handle kind the caller actually holds and the
			// paths that exist on it: the earlier message suggested /items, which
			// does not exist on any receipt, and the recorded loop followed that
			// advice five times into the same "path does not exist" wall.
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(receipt.Data, &fields)
			paths := []string{}
			for key := range fields {
				paths = append(paths, "/"+key)
			}
			sort.Strings(paths)
			return ToolResult{}, fmt.Errorf("handle %q belongs to %s, not a fetched page. Use view=json with an actual pointer from %v; to inspect a page use its inspect_source handle", a.Handle, op.Kind, paths)
		}
		if err := json.Unmarshal(receipt.Data, &value); err != nil {
			return ToolResult{}, err
		}
		// An empty pointer hands back receipt internals (artifact hashes and the
		// like) that say nothing about the task. Point at the business content.
		if a.Pointer == "" {
			paths := []string{}
			if root, ok := value.(map[string]any); ok {
				for key := range root {
					paths = append(paths, "/"+pointerKey(key))
				}
			}
			sort.Strings(paths)
			meta["available_pointers"] = paths
			meta["pointer_hint"] = "Choose an actual pointer in available_pointers; nested values carry their exact read references."
		}
	} else {
		page, err := s.inspection(ctx, run, a.Handle)
		if err != nil {
			return ToolResult{}, err
		}
		meta["url"], meta["fetched_at"] = page.Source.Receipt.URL, page.Source.Receipt.FetchedAt
		if a.View == "" {
			// Structural by default. Whole-page prose cannot express a field, so
			// defaulting to it made a reading model read and re-read without ever
			// producing a value; the selector inventory answers the question a
			// crawler asks. The prose projection stays in the host for RAG internals only.
			//
			// A model-supplied view=content is refused at the read_data entry
			// point, before this dispatcher runs; by the time a call reaches here
			// the view was chosen by the host for a path that needs text (a
			// body-field read, for example), so content still works internally.
			a.View = "dom"
		}
		switch a.View {
		case "receipt":
			if a.Selector != "" || a.BlockID != "" {
				return ToolResult{}, fmt.Errorf("receipt view cannot use a selector or block")
			}
			view, err := sourceReadingView(page)
			if err != nil {
				return ToolResult{}, err
			}
			raw, _ := json.Marshal(view)
			_ = json.Unmarshal(raw, &value)
			version = "inspection-receipt-v1"
		case "json":
			version = "json-value-v1"
			if a.Selector != "" {
				return ToolResult{}, fmt.Errorf("view=json reads structured values by path, so a CSS selector does not apply. Use {\"handle\":%q,\"view\":\"json\",\"pointer\":\"/items\"}, or switch to view=content with the selector to read page text", a.Handle)
			}
			if a.BlockID == "" {
				if err := json.Unmarshal([]byte(page.Source.Body), &value); err != nil {
					// The page is HTML, so its JSON lives in an embedded block. A
					// caller that omits block_id is asking for the embedded value,
					// not making an error: bind the first embedded block and say
					// which one, instead of refusing and leaving it to guess an id.
					info, infoErr := buildHTMLInformation(page.Source.Body, page.Source.Receipt.URL, "")
					if infoErr != nil || info == nil || len(info.Embedded) == 0 {
						return ToolResult{}, toolContractFailure(fmt.Errorf("this source is HTML with no embedded JSON"), "SOURCE_NOT_JSON", "not_started", map[string]any{"field": "view", "read_with": map[string]any{"handle": a.Handle, "view": "dom"}}, ToolRecovery{Strategy: "inspect_and_correct", Tool: "read_data", Instruction: "Use view=dom to inspect observed selectors, then choose the required region."})
					}
					part := info.Embedded[0]
					a.BlockID = part.ID
					meta["block_id"], meta["selector"] = part.ID, part.Selector
					meta["embedded_total"] = len(info.Embedded)
					if err := json.Unmarshal(part.Value, &value); err != nil {
						return ToolResult{}, err
					}
				}
			} else {
				info, err := buildHTMLInformation(page.Source.Body, page.Source.Receipt.URL, "")
				if err != nil {
					return ToolResult{}, err
				}
				if info == nil {
					return ToolResult{}, fmt.Errorf("embedded JSON requires HTML")
				}
				found := false
				for _, part := range info.Embedded {
					if part.ID == a.BlockID {
						found = true
						meta["selector"] = part.Selector
						if err := json.Unmarshal(part.Value, &value); err != nil {
							return ToolResult{}, err
						}
						break
					}
				}
				if !found {
					return ToolResult{}, fmt.Errorf("embedded JSON block not found")
				}
				version = "html-embedded-json-v1"
			}
		case "content":
			// A pointer addresses the receipt's structured fields (for example
			// /structure/0/selector), not the rendered text. The recorded run
			// asked exactly that with view=content, hit "path crosses a scalar"
			// three times, never saw the real selectors, and spent the rest of
			// its budget guessing invented ones. Resolve the pointer against the
			// receipt so the intent succeeds instead of erroring.
			if a.Pointer != "" && a.BlockID == "" {
				// A pointer addresses the receipt's structured fields (for example
				// /structure/0/selector), not the rendered text. Resolve the
				// pointer against the receipt and page it as data, exactly as the
				// recorded run's successful path did.
				view, viewErr := sourceReadingView(page)
				if viewErr != nil {
					return ToolResult{}, viewErr
				}
				raw, _ := json.Marshal(view)
				_ = json.Unmarshal(raw, &value)
				version = "inspection-receipt-v1"
				meta["resolved_from"] = "receipt (pointer addresses receipt fields, not page text)"
				break
			}
			info, err := buildHTMLInformation(page.Source.Body, page.Source.Receipt.URL, a.Selector)
			if err != nil {
				// A selector that matches nothing is a guess at site markup, and a
				// caller that only learns "no match" tries another guess. Name the
				// selectors this source really contains, so the next attempt is a
				// choice rather than another guess.
				if a.Selector != "" {
					return ToolResult{}, sourceSelectorFailure(err, page.Source.Body, a)
				}
				return ToolResult{}, err
			}
			version = "plain-text-v1"
			value = page.Source.Body
			if info != nil {
				version = info.Version
				value = info.Text()
				meta["selector"], meta["matches"] = a.Selector, info.Matches
				// A long page is grouped into headed regions, and the region a
				// caller wants is often not the first one: on a listing page the
				// opening region can be site news while the requested rows sit
				// several thousand runes later. Without an index the only way to
				// reach them is to page through everything before them, which a
				// bounded run cannot afford. The index names each heading region
				// with the rune range it occupies, so a caller can jump straight
				// to the relevant region instead of reading past the rest.
				if a.BlockID == "" {
					if index := blockDirectory(info); len(index) > 0 {
						meta["regions"] = index
						meta["regions_note"] = "Each region lists its heading and the rune range it occupies in this view. Read the region you need by passing its start_offset; continue inside a region with next_read."
					}
				}
				if a.BlockID != "" {
					// IDs are global to the full receipt, never a selected region.
					if a.Selector != "" {
						return ToolResult{}, fmt.Errorf("block_id and selector are separate scopes")
					}
					found := false
					for _, block := range info.Blocks {
						if block.ID != a.BlockID {
							continue
						}
						found = true
						// Present the block's own fields at the top level, the same
						// shape every other read uses. Nesting them under value made
						// the top-level text empty, so a caller looking for text found
						// nothing and re-read the same block indefinitely.
						meta["kind"], meta["selector"], meta["context"] = block.Kind, block.Selector, block.Context
						meta["headers"], meta["caption"] = block.Headers, block.Caption
						meta["block_id"] = block.ID
						if a.Pointer != "" {
							var value any
							raw, _ := json.Marshal(block)
							_ = json.Unmarshal(raw, &value)
							selected, err := jsonPointer(value, a.Pointer)
							if err != nil {
								return ToolResult{}, err
							}
							a.ViewVersion = version
							meta["view_version"] = version
							meta["pointer"] = a.Pointer
							return informationValuePage(call, a, selected, meta)
						}
						meta["text"] = block.Text
						meta["total_runes"] = len([]rune(block.Text))
						if len(block.Links) > 0 {
							meta["links"] = block.Links
						}
						if len(block.Cells) > 0 {
							meta["cells"] = block.Cells
							entity := []rune(block.Cells[0].Text)
							meta["row_entity_preview"] = string(entity[:min(120, len(entity))])
						}
						// Do not duplicate the entire row under value. Large text/cells
						// get exact block-relative readers, not a rejected whole block.
						if len([]byte(block.Text)) > 1024 {
							meta["text"] = fitObservationText(block.Text, 1024)
							meta["text_truncated"] = true
							meta["read_text_with"] = map[string]any{"handle": a.Handle, "view": "content", "block_id": block.ID, "pointer": "/text"}
						}
						if len(block.Cells) > 0 {
							cells := []map[string]any{}
							for index, cell := range block.Cells {
								cells = append(cells, map[string]any{"text": informationChild(cell.Text, a, "/cells/"+strconv.Itoa(index)+"/text"), "colspan": cell.Colspan, "rowspan": cell.Rowspan})
							}
							meta["cells"] = cells
						}
						break
					}
					if !found {
						return ToolResult{}, fmt.Errorf("content block not found")
					}
					// The block was already placed in meta with its own fields at
					// the top level. Return it directly: paging a whole-page value
					// here would overwrite text with the page's opening slice.
					if a.ViewVersion != "" && a.ViewVersion != version {
						return ToolResult{}, fmt.Errorf("reader version changed; restart reading")
					}
					meta["view_version"] = version
					return boundedReadResult(call, meta)
				}
			} else if a.BlockID != "" || a.Selector != "" {
				return ToolResult{}, fmt.Errorf("block or CSS selection requires HTML")
			}
		case "dom":
			if a.BlockID != "" || a.Pointer != "" {
				return ToolResult{}, fmt.Errorf("DOM inspection uses a selector or catalog offset, not a block or JSON pointer")
			}
			if a.ViewVersion != "" && a.ViewVersion != "dom-structure-v1" {
				return ToolResult{}, fmt.Errorf("reader version changed; restart reading")
			}
			a.ViewVersion = "dom-structure-v1"
			if a.Selector == "" {
				catalog := completeSourceSelectorCatalog(page.Source.Body)
				if a.Offset > len(catalog) {
					return ToolResult{}, fmt.Errorf("catalog offset %d exceeds %d selectors", a.Offset, len(catalog))
				}
				limit := a.Limit
				if limit == 0 {
					limit = 64
				}
				end := a.Offset
				selected := []DOMBlock{}
				for end < len(catalog) && end-a.Offset < limit {
					if !contextManaged {
						candidate := append(append([]DOMBlock{}, selected...), catalog[end])
						raw, _ := json.Marshal(candidate)
						if len(raw) > observationContentBytes {
							break
						}
					}
					selected = append(selected, catalog[end])
					end++
				}
				if end == a.Offset && end < len(catalog) {
					return ToolResult{}, fmt.Errorf("single DOM locator exceeds observation size; inspect a narrower source region")
				}
				meta["selector_catalog"], meta["offset"], meta["total_selectors"] = selected, a.Offset, len(catalog)
				if end < len(catalog) {
					next := a
					next.Offset = end
					next.RunID, next.ReceiptID = "", 0
					meta["next_read"] = next
					meta["next_cursor"] = nextReadCursor(a, end)
				}
				meta["view_version"] = "dom-structure-v1"
				meta["next_step"] = "Choose a selector from this observed inventory; view=dom plus selector reads its nodes. Follow next_read to inspect remaining selectors. DOM offsets count inventory entries or matched nodes, not text runes."
				return boundedReadResult(call, meta, contextManaged)
			}
			info, err := buildHTMLInformation(page.Source.Body, page.Source.Receipt.URL, a.Selector)
			if err != nil {
				return ToolResult{}, sourceSelectorFailure(err, page.Source.Body, a)
			}
			if info == nil {
				return ToolResult{}, fmt.Errorf("DOM view requires HTML")
			}
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(page.Source.Body))
			if err != nil {
				return ToolResult{}, err
			}
			selection := doc.Find(a.Selector)
			if a.Offset > selection.Length() {
				return ToolResult{}, fmt.Errorf("DOM offset %d exceeds %d matched nodes", a.Offset, selection.Length())
			}
			sample, truncated := structuralDOMSample(selection.First())
			meta["view_version"], meta["structural_only"], meta["selector"], meta["matches"], meta["sample_truncated"] = "dom-structure-v1", true, a.Selector, info.Matches, truncated
			meta["html_sample"] = sample
			// A selector denotes a collection, not only its first element. Keep
			// text/attributes/index together so callers need not generate a Lua
			// probe merely to enumerate the matches they already selected.
			limit := a.Limit
			if limit == 0 {
				limit = 64
			}
			byteLimit := observationContentBytes
			if contextManaged {
				byteLimit = 0
			}
			nodes := domRowNodes(page.Source.Receipt.URL, selection, a.Offset, limit, byteLimit)
			end := a.Offset + len(nodes)
			meta["nodes"], meta["offset"], meta["nodes_truncated"] = nodes, a.Offset, end < selection.Length()
			if end < selection.Length() {
				next := a
				next.Offset = end
				next.RunID, next.ReceiptID = "", 0
				meta["next_read"] = next
				meta["next_cursor"] = nextReadCursor(a, end)
			}
			if a.ViewVersion != "" && a.ViewVersion != "dom-structure-v1" {
				return ToolResult{}, fmt.Errorf("reader version changed; restart reading")
			}
			meta["narrow_with"] = "nodes retain source order and absolute zero-based indexes under this selector. Follow next_read for remaining nodes; view=content with this selector reads its complete visible text."
			return boundedReadResult(call, meta, contextManaged)
		default:
			return ToolResult{}, fmt.Errorf("unknown information view")
		}
	}
	if a.ViewVersion != "" && a.ViewVersion != version {
		return ToolResult{}, fmt.Errorf("reader version changed; restart reading")
	}
	// An offset into HTML must say which projection produced it, or the same
	// offset could mean a different span after a reader change. A caller that
	// omits the version is not making a stale claim; it is reading the current
	// projection, so bind the current version rather than refusing. Refusing here
	// made every offset read without an explicit version fail, which the model
	// answered by restarting at zero.
	a.ViewVersion = version
	meta["view_version"], meta["pointer"] = version, a.Pointer
	if a.BlockID != "" {
		meta["block_id"] = a.BlockID
	}
	selected, err := jsonPointer(value, a.Pointer)
	if err != nil {
		return ToolResult{}, err
	}
	return informationValuePage(call, a, selected, meta)
}

func boundedReadResult(call ToolCall, data map[string]any, contextManaged ...bool) (ToolResult, error) {
	out, err := toolData(call, data)
	if err == nil && (len(contextManaged) == 0 || !contextManaged[0]) && len(out.Data) > readerObservationBytes {
		return ToolResult{}, fmt.Errorf("read metadata exceeds observation budget; narrow the selector/path (explicit gap)")
	}
	return out, err
}

func informationChild(value any, a dataReadArgs, pointer string) any {
	raw, _ := json.Marshal(value)
	if len(raw) <= 1024 {
		return value
	}
	a.Pointer = pointer
	a.Offset = 0
	a.Limit = 0
	a.Cursor = ""
	ref := map[string]any{"omitted": true, "read_with": map[string]any{"cursor": nextReadCursor(a, 0)}}
	switch v := value.(type) {
	case string:
		ref["type"], ref["total_runes"] = "text", len([]rune(v))
	case []any:
		ref["type"], ref["total_items"] = "array", len(v)
	case map[string]any:
		ref["type"], ref["total_items"] = "object", len(v)
	}
	return ref
}

// Reading is bounded by the observation budget, and the continuation cursor is
// base64 of the whole read request, so it competes with the text it continues.
// These floors keep one page readable in a small number of calls: without them a
// 10k-character page needed roughly fifty reads, which no progress window allows.
const (
	defaultReadRuness = 1200
	minReadRuness     = 400
)

// blockDirectory summarises the headed regions of a converted page as rune
// ranges in the rendered view. It exists because a caller that needs a later
// region would otherwise have to page through every earlier one, and a bounded
// run cannot afford that. It reads only the projection, never the source.
func blockDirectory(info *HTMLInformation) []map[string]any {
	if info == nil || len(info.Blocks) == 0 {
		return nil
	}
	out := []map[string]any{}
	position := 0
	currentHeading := ""
	regionStart := 0
	flush := func(end int) {
		if end <= regionStart {
			return
		}
		out = append(out, map[string]any{
			"heading":      currentHeading,
			"start_offset": regionStart,
			"end_offset":   end,
		})
	}
	// Walk in the same order Text() emits blocks so ranges match the paged value.
	for _, kind := range []string{"content", "link", "code", "embedded_json"} {
		for _, block := range info.Blocks {
			group := block.Kind
			if group != "link" && group != "code" && group != "embedded_json" {
				group = "content"
			}
			if group != kind {
				continue
			}
			if block.Kind == "heading" {
				flush(position)
				currentHeading = block.Text
				regionStart = position
			}
			position += len([]rune(renderBlockText(block)))
		}
	}
	flush(position)
	// Keep the directory small: a bounded run only needs to know where the
	// distinct regions are, not every block.
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// renderBlockText mirrors one block's contribution to HTMLInformation.Text().
func renderBlockText(block HTMLContentBlock) string {
	var text strings.Builder
	text.WriteString("[" + block.Kind + " " + block.ID + "]")
	if len(block.Context) > 0 {
		text.WriteString(" " + strings.Join(block.Context, " > "))
	}
	if block.Class != "" {
		text.WriteString(" (" + block.Class + ")")
	}
	text.WriteString("\n" + block.Text)
	for _, link := range block.Links {
		text.WriteString("\n" + link.Text + " → " + link.Href)
	}
	text.WriteString("\n\n")
	return text.String()
}

func informationValuePage(call ToolCall, a dataReadArgs, value any, meta map[string]any) (ToolResult, error) {
	meta["offset"], meta["next_cursor"] = a.Offset, nil
	count := a.Limit
	switch v := value.(type) {
	case string:
		runes := []rune(v)
		if a.Offset > len(runes) {
			return ToolResult{}, fmt.Errorf("offset outside text")
		}
		if count == 0 {
			count = defaultReadRuness
		}
		end := min(a.Offset+count, len(runes))
		meta["total_runes"] = len(runes)
		for {
			meta["text"], meta["next_cursor"] = string(runes[a.Offset:end]), nil
			if end < len(runes) {
				// Publish both continuations. next_read is the short one a model
				// should echo; next_cursor is the self-contained one the host and
				// read_observation use to compute the next offset. Emitting only the
				// short form left next_offset unset on the observation path.
				meta["next_cursor"] = nextReadCursor(a, end)
				meta["next_read"] = map[string]any{"continue_from": call.ID}
				meta["unread_runes"] = len(runes) - end
				meta["continue_with"] = "call read_data again with next_read exactly as returned (continue_from), or with next_cursor; passing only a handle restarts at offset 0 and returns this same slice"
			}
			out, err := boundedReadResult(call, meta)
			if err == nil {
				return out, nil
			}
			if end-a.Offset <= 1 {
				return ToolResult{}, err
			}
			// Halving on overflow used to shrink a whole page into ~200-character
			// slices, because the cursor payload competes with the text for the
			// same observation budget. A 10k-character page then needed ~51 calls,
			// far more than any progress window allows, so the model could never
			// finish reading and the run always ended as failed delivery.
			// Shrink toward a floor that stays useful instead of toward one rune.
			next := a.Offset + (end-a.Offset)/2
			if next-a.Offset < minReadRuness && end-a.Offset > minReadRuness {
				next = a.Offset + minReadRuness
			}
			end = next
		}
	case []any:
		if a.Offset > len(v) {
			return ToolResult{}, fmt.Errorf("offset outside array")
		}
		if count == 0 {
			count = 4
		}
		end := min(a.Offset+min(count, 8), len(v))
		items := []any{}
		for n := a.Offset; n < end; n++ {
			items = append(items, informationChild(v[n], a, a.Pointer+"/"+strconv.Itoa(n)))
		}
		meta["value"], meta["total_items"] = items, len(v)
		if end < len(v) {
			meta["next_cursor"] = nextReadCursor(a, end)
			meta["next_read"] = map[string]any{"continue_from": call.ID}
		}
	case map[string]any:
		keys := []string{}
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if a.Offset > len(keys) {
			return ToolResult{}, fmt.Errorf("offset outside object")
		}
		if count == 0 {
			count = 8
		}
		end := min(a.Offset+min(count, 8), len(keys))
		fields := map[string]any{}
		for _, key := range keys[a.Offset:end] {
			fields[key] = informationChild(v[key], a, a.Pointer+"/"+pointerKey(key))
		}
		meta["value"], meta["total_items"] = fields, len(keys)
		if end < len(keys) {
			meta["next_cursor"] = nextReadCursor(a, end)
			meta["next_read"] = map[string]any{"continue_from": call.ID}
		}
	default:
		if a.Offset != 0 {
			return ToolResult{}, fmt.Errorf("scalar has no offset")
		}
		meta["value"] = value
	}
	out, err := boundedReadResult(call, meta)
	if err != nil {
		// Reduce collection width, not child contents. Every omitted child keeps
		// its exact address; the next cursor advances by the actual emitted count.
		if _, ok := value.([]any); ok && count > 1 {
			a.Limit = max(1, count/2)
			return informationValuePage(call, a, value, meta)
		}
		if _, ok := value.(map[string]any); ok && count > 1 {
			a.Limit = max(1, count/2)
			return informationValuePage(call, a, value, meta)
		}
	}
	return out, err
}

// selectorCandidates lists selectors this source actually contains, ranked by
// how much list-like content they hold. It exists because a caller that only
// learns "no match" responds by guessing again: the recorded run tried six
// invented selectors and then repeated one selector fifteen times, spending
// almost its entire round budget on misses. Naming real candidates turns the
// next attempt into a choice. Only structure is read; no value is interpreted.
func selectorCandidates(body string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return "(could not parse this source)"
	}
	doc.Find("script,style,noscript,template").Remove()
	type candidate struct {
		sel   string
		links int
	}
	seen := map[string]bool{}
	found := []candidate{}
	// Container shapes that commonly hold repeated rows on listing pages.
	for _, sel := range []string{"ul li", "ol li", "table tr", "div.list li", ".list li", "li a", "li p", "li p a", "p a", "dl dt", "article"} {
		nodes := doc.Find(sel)
		if nodes.Length() == 0 {
			continue
		}
		links := nodes.Find("a").Length()
		if seen[sel] {
			continue
		}
		seen[sel] = true
		found = append(found, candidate{sel, links})
	}
	if len(found) == 0 {
		return "(no repeated list structure found; inspect view=dom for observed regions and read a chosen selector)"
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].links != found[j].links {
			return found[i].links > found[j].links
		}
		return found[i].sel < found[j].sel
	})
	out := []string{}
	for _, c := range found {
		if len(out) >= 6 {
			break
		}
		if c.links > 0 {
			out = append(out, fmt.Sprintf("%q (%d links)", c.sel, c.links))
		} else {
			out = append(out, fmt.Sprintf("%q", c.sel))
		}
	}
	return strings.Join(out, ", ")
}

func sourceSelectorFailure(cause error, html string, a dataReadArgs) error {
	return toolContractFailure(
		fmt.Errorf("selector %q: %w; read_data(handle=%q,view=dom) lists actual selectors", a.Selector, cause, a.Handle),
		"SOURCE_SELECTOR_NOT_FOUND", "not_started",
		map[string]any{"field": "selector", "actual": a.Selector, "selector_catalog": crawlerCatalog(html, sourceSelectorCatalog(html)), "read_with": map[string]any{"handle": a.Handle, "view": "dom"}},
		ToolRecovery{Strategy: "inspect_and_correct", Tool: "read_data", Instruction: "Read the actual selector catalog for this source (view=dom with no selector) and correct the selector."})
}
