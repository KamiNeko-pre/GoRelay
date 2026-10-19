package aiapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
)

// Locators are untrusted proposals. Values are read again from the exact host
// response; a quote provided by Lua cannot manufacture support.
type fieldLocator struct {
	SourceURL      string  `json:"source_url,omitempty"`
	JSONKeyPointer *string `json:"json_key_pointer,omitempty"`
	Selector       string  `json:"selector,omitempty"`
	Index          int     `json:"index,omitempty"` // zero based; one node only, no global substring search.
	Attribute      string  `json:"attribute,omitempty"`
	JSONPointer    *string `json:"json_pointer,omitempty"`
	Pattern        string  `json:"pattern,omitempty"` // optional RE2 capture of original text, no value conversion.
}

func normalizeEvidenceText(s string) string { return strings.Join(strings.Fields(s), " ") }

type fieldLocatorError struct {
	Code  string
	Cause error
}

func (e *fieldLocatorError) Error() string { return e.Cause.Error() }
func locatorError(code, message string) error {
	return &fieldLocatorError{Code: code, Cause: fmt.Errorf("%s", message)}
}
func locatorWrappedError(code string, cause error) error {
	return &fieldLocatorError{Code: code, Cause: cause}
}

func evidenceValue(source acquisition.Response, loc fieldLocator) (any, string, error) {
	if loc.SourceURL != "" && loc.SourceURL != source.Receipt.URL && loc.SourceURL != source.Receipt.RequestedURL {
		return nil, "", locatorError("SOURCE_NOT_OBSERVED", "field source_url does not match this observed response")
	}
	if loc.JSONPointer != nil || loc.JSONKeyPointer != nil {
		if loc.JSONPointer != nil && loc.JSONKeyPointer != nil || loc.Selector != "" || loc.Attribute != "" || loc.Pattern != "" {
			return nil, "", locatorError("MIXED_LOCATOR_KINDS", "mixed JSON/DOM locator")
		}
		var value any
		d := json.NewDecoder(strings.NewReader(source.Body))
		d.UseNumber()
		if err := d.Decode(&value); err != nil {
			return nil, "", locatorWrappedError("INVALID_JSON_SOURCE", err)
		}
		if loc.JSONKeyPointer != nil {
			p := *loc.JSONKeyPointer
			at := strings.LastIndex(p, "/")
			if at < 0 {
				return nil, "", locatorError("JSON_OBJECT_MEMBER_REQUIRED", "json_key_pointer must name an existing object member")
			}
			parent, err := jsonPointer(value, p[:at])
			if err != nil {
				return nil, "", locatorWrappedError("JSON_POINTER_NOT_FOUND", err)
			}
			object, ok := parent.(map[string]any)
			if !ok {
				return nil, "", locatorError("JSON_OBJECT_MEMBER_REQUIRED", "json_key_pointer parent must be an object, not an array or scalar")
			}
			key := strings.ReplaceAll(strings.ReplaceAll(p[at+1:], "~1", "/"), "~0", "~")
			if _, ok := object[key]; !ok {
				return nil, "", locatorError("JSON_POINTER_NOT_FOUND", "JSON object member not found")
			}
			return key, "json-key:" + p, nil
		}
		value, err := jsonPointer(value, *loc.JSONPointer)
		if err != nil {
			return nil, "", locatorWrappedError("JSON_POINTER_NOT_FOUND", err)
		}
		return value, "json:" + *loc.JSONPointer, nil
	}
	if loc.Selector == "" || len(loc.Selector) > 512 || loc.Index < 0 {
		return nil, "", locatorError("DOM_LOCATOR_REQUIRED", "local DOM locator required")
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(source.Body))
	if err != nil {
		return nil, "", locatorWrappedError("INVALID_DOM_SOURCE", err)
	}
	doc.Find("script,style,noscript,template").Remove()
	return domEvidenceValue(doc, loc)
}
func domEvidenceValue(doc *goquery.Document, loc fieldLocator) (value any, ref string, err error) {
	nodes := doc.Find(loc.Selector)
	if nodes.Length() <= loc.Index {
		return nil, "", locatorError("DOM_NODE_NOT_FOUND", "DOM locator not found")
	}
	node := nodes.Eq(loc.Index)
	text := node.Text()
	if loc.Attribute != "" {
		var ok bool
		text, ok = node.Attr(loc.Attribute)
		if !ok {
			return nil, "", locatorError("DOM_ATTRIBUTE_NOT_FOUND", "attribute not found")
		}
	}
	text = normalizeEvidenceText(text)
	if loc.Pattern != "" {
		if len(loc.Pattern) > 512 {
			return nil, "", locatorError("CAPTURE_PATTERN_LIMIT", "capture pattern too long")
		}
		re, e := regexp.Compile(loc.Pattern)
		if e != nil {
			return nil, "", locatorWrappedError("INVALID_CAPTURE_PATTERN", e)
		}
		matches := re.FindAllStringSubmatch(text, 2)
		if len(matches) != 1 || len(matches[0]) != 2 {
			return nil, "", locatorError("CAPTURE_NOT_UNIQUE", "pattern requires exactly one match and capture")
		}
		text = matches[0][1]
	}
	return text, fmt.Sprintf("dom:%s[%d]@%s", loc.Selector, loc.Index, loc.Attribute), nil
}
func hostFieldSupport(result engine.ProgramResult, sourceURL string, fields map[string]any, proposed any) map[string]FieldSupport {
	out := map[string]FieldSupport{}
	locators, _ := proposed.(map[string]any)
	for name, value := range fields {
		unsupported := func(code, reason string, loc *fieldLocator) {
			out[name] = FieldSupport{Status: "unsupported", CheckerVersion: "host-locator-v2", DiagnosticCode: code, ActualLocator: loc, Reason: truncateRunes(reason, 256)}
		}
		if name == "source_url" {
			out[name] = FieldSupport{Status: "supported", CheckerVersion: "host-locator-v2", EvidenceRefs: []string{sourceURL + "#successful-get"}}
			continue
		}
		locRaw, hasLoc := locators[name]
		var loc fieldLocator
		if hasLoc {
			raw, err := json.Marshal(locRaw)
			if err != nil {
				unsupported("LOCATOR_INVALID", "invalid field locator: "+err.Error(), nil)
				continue
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&loc); err != nil {
				unsupported("LOCATOR_INVALID", "invalid field locator: "+err.Error(), nil)
				continue
			}
		}
		target := sourceURL
		if loc.SourceURL != "" {
			target = loc.SourceURL
		}
		if text, ok := value.(string); ok {
			for _, source := range result.SourceEvidence {
				if !observedCrawlerSource(source) || (source.Receipt.URL != target && source.Receipt.RequestedURL != target) {
					continue
				}
				full, err := acquisition.SourceText(source.Body)
				if err == nil && full != "" && normalizeEvidenceText(text) == full {
					out[name] = FieldSupport{Status: "supported", FullSource: true, CheckerVersion: "host-full-source-v1", EvidenceRefs: []string{source.Receipt.URL + "#complete-visible-source"}, SourceDigest: agentdata.Hash(source.Body), Reason: "exact full visible text of fetched response; external links and attachments not implied"}
					break
				}
			}
			if out[name].FullSource {
				continue
			}
		}
		if !hasLoc {
			unsupported("LOCATOR_MISSING", "missing _evidence."+name+" locator", nil)
			continue
		}
		unsupported("SOURCE_NOT_OBSERVED", "no matching successful GET evidence", &loc)
		for _, source := range result.SourceEvidence {
			if !observedCrawlerSource(source) || (source.Receipt.URL != target && source.Receipt.RequestedURL != target) {
				continue
			}
			actual, ref, err := evidenceValue(source, loc)
			if err != nil {
				var detail *fieldLocatorError
				code := "LOCATOR_INVALID"
				if errors.As(err, &detail) {
					code = detail.Code
				}
				unsupported(code, err.Error(), &loc)
				s := out[name]
				s.SourceDigest = agentdata.Hash(source.Body)
				out[name] = s
				continue
			}
			if matchingFieldValue(value, actual) {
				out[name] = FieldSupport{Status: "supported", CheckerVersion: "host-locator-v2", EvidenceRefs: []string{source.Receipt.URL + "#" + ref}, SourceDigest: agentdata.Hash(source.Body), Reason: "mechanical source match; semantic meaning is evaluated separately"}
				break
			}
			unsupported("VALUE_MISMATCH", "source locator value differs from proposed field value", &loc)
			s := out[name]
			s.SourceDigest = agentdata.Hash(source.Body)
			out[name] = s
		}
	}
	for name, support := range out {
		if support.Status == "unsupported" {
			support.SuggestedLocator = verifiedLocatorSuggestion(result, sourceURL, fields[name])
			out[name] = support
		}
	}
	return out
}

func requireSupportedFields(spec TaskSpec, records []Record) error {
	if !spec.RequireFieldSupport {
		return nil
	}
	type unsupportedField struct {
		Field         string       `json:"field"`
		SourceURL     string       `json:"source_url"`
		ActualSupport FieldSupport `json:"actual_support"`
		Reason        string       `json:"reason"`
		Records       int          `json:"affected_records"`
	}
	// One example per distinct field/diagnostic explains the whole repair;
	// repeating an identical missing locator on every row adds no new guidance.
	issues := map[string]*unsupportedField{}
	fieldSet := map[string]bool{}
	affectedRecords := 0
	for _, record := range records {
		unsupportedRecord := false
		for name := range record.Fields {
			if !fieldIsSupported(record.Provenance, name) {
				fieldSet[name] = true
				unsupportedRecord = true
				reason := record.Provenance.FieldSupport[name].Reason
				if reason == "" {
					reason = "provide a local DOM/JSON locator in _evidence"
				}
				key := name + "\x00" + reason
				if issues[key] == nil {
					issues[key] = &unsupportedField{Field: name, SourceURL: record.Provenance.URL, ActualSupport: record.Provenance.FieldSupport[name], Reason: reason}
				}
				issues[key].Records++
			}
		}
		if unsupportedRecord {
			affectedRecords++
		}
	}
	if len(issues) == 0 {
		return nil
	}
	keys := make([]string, 0, len(issues))
	for key := range issues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	all := make([]unsupportedField, 0, len(keys))
	for _, key := range keys {
		all = append(all, *issues[key])
	}
	fields := make([]string, 0, len(fieldSet))
	for name := range fieldSet {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	first := all[0]
	return toolContractFailure(fmt.Errorf("unsupported field evidence: %d fields across %d records: %s; first example %s: %s. Correct every entry in unsupported_fields", len(fields), affectedRecords, strings.Join(fields, ", "), first.Field, first.Reason), "FIELD_EVIDENCE_UNSUPPORTED", "failed", map[string]any{"field": first.Field, "fields": fields, "unsupported_field_count": len(fields), "unsupported_record_count": affectedRecords, "source_url": first.SourceURL, "actual_support": first.ActualSupport, "unsupported_fields": all, "expected": "exact value from a local source locator", "locator_contract": "Lua html_* and _evidence use the same goquery/Cascadia CSS grammar; index is zero-based on each item's source_url response"}, ToolRecovery{Strategy: "correct_field_locator", Instruction: "Correct every entry in unsupported_fields. When actual_support.suggested_locator exists, the host verified its exact value on that source_url; use it for extraction and copy it into the returned item's _evidence[field]. This is a mechanical match, so still check the field's meaning and whether the selector applies to other detail pages. Otherwise use saved structure to choose selector/index/attribute or json_pointer. For submit_extraction prefer field_selectors. For Lua preserve acquisition/pagination code, attach _evidence to each returned item, then validate the changed program. Set repair_of to this call only when retrying this same tool; omit cross-tool repair_of. Do not repeat the unchanged call, invent values or reread article text.", Steps: []RecoveryStep{
		{Action: "review_all_field_failures", Instruction: "Use fields and unsupported_fields as the complete correction list; the first example is not the only failed field. Each entry names its source, cause and any verified locator."},
		{Action: "pair_values_and_locators", Instruction: "For Lua, put each field value and its DOM/attribute/JSON locator together in that item's _evidence table, relative to its source_url. For direct extraction, use field_selectors or correct all supplied item locators. Preserve successful navigation and source values."},
		{Action: "submit_corrected_proposal", Instruction: "Submit the genuinely corrected program/extraction through the same tool. A same-tool failed-call reference can link this correction; keep cross-tool repair_of absent. Follow the resulting successful publication/submission continuation."},
	}})
}
