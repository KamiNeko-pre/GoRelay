package information

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
)

// deliveryEvidence locates a window in an immutable, task-linked snapshot.
type deliveryEvidence struct {
	RequestID string `gorm:"primaryKey;size:32"`
	ID        string `gorm:"primaryKey;size:24"`
	DocID     string `gorm:"size:64"`
	Revision  string `gorm:"size:64"`
	Raw       bool
	Start     int
	End       int
}

func newDeliveryEvidence(requestID string, doc knowledge.Document, raw bool, start, end int) deliveryEvidence {
	id := "ev_" + agentdata.Hash([]any{doc.ID, doc.Revision, raw, start, end})[:20]
	return deliveryEvidence{RequestID: requestID, ID: id, DocID: doc.ID, Revision: doc.Revision, Raw: raw, Start: start, End: end}
}

func resolveDeliveryDocument(ctx context.Context, store *knowledge.Store, version [2]string, scope knowledge.RetrievalScope, documents map[[2]string]knowledge.Document) (knowledge.Document, error) {
	if doc, exists := documents[version]; exists {
		return doc, nil
	}
	doc, err := store.ResolveDocument(ctx, version[0], version[1], scope)
	if err == nil {
		documents[version] = doc
	}
	return doc, err
}

// Resolve a number at its observed path in an exact task-linked response. The
// parent delivery transaction owns both the access check and the parsed cache.
func resolveJSONDeliveryEvidence(ctx context.Context, tx *gorm.DB, requestID string, scope knowledge.RetrievalScope, location JSONCalculationEvidence, documents map[[2]string]knowledge.Document, bodies map[[2]string]any) (float64, StoredReference, map[string]string, error) {
	ref := StoredReference{DocID: location.SourceRef.DocID, Revision: location.SourceRef.Revision}
	version := [2]string{ref.DocID, ref.Revision}
	root, loaded := bodies[version]
	if !loaded {
		if _, err := validateDeliveryLinks(tx, requestID, []StoredReference{ref}); err != nil {
			return 0, ref, nil, err
		}
		doc, err := resolveDeliveryDocument(ctx, knowledge.NewStore(tx), version, scope, documents)
		if err != nil {
			return 0, ref, nil, err
		}
		body := doc.RawBody
		if body == "" {
			body = doc.Body
		}
		var ok bool
		root, ok = decodeJSONCalculationSource(body)
		if !ok {
			return 0, ref, nil, fmt.Errorf("快照 %s 不是完整 JSON；从可用原文提取有效片段并用 evidence_id 引用", ref.DocID)
		}
		bodies[version] = root
	}
	path, err := normalizeJSONPointer(location.JSONPointer)
	if err != nil {
		return 0, ref, nil, err
	}
	value, err := pointer(root, path)
	if err != nil {
		return 0, ref, nil, err
	}
	number, ok := deliveryJSONNumber(value)
	if !ok {
		return 0, ref, nil, fmt.Errorf("JSON Pointer %q 指向 %T，无法作为数值；选择实际的数字字段或以文字引用规则", path, value)
	}
	return number, ref, map[string]string{
		"url": documents[version].URL, "doc_id": ref.DocID, "revision": ref.Revision,
		"json_pointer": path, "excerpt": string(mustMarshal(value)),
	}, nil
}
func decodeJSONCalculationSource(body string) (any, bool) {
	var root any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false
	}
	return root, true
}

func deliveryJSONNumber(value any) (float64, bool) {
	var token string
	switch v := value.(type) {
	case string:
		token = strings.TrimSpace(v)
	case json.Number:
		token = v.String()
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	match := deliveryNumber.FindStringIndex(token)
	number, err := strconv.ParseFloat(token, 64)
	return number, !(match == nil || match[0] != 0 || match[1] != len(token) || err != nil || math.IsNaN(number) || math.IsInf(number, 0))
}

// The persisted offsets count runes. Find the two byte boundaries without
// allocating a rune array for the rest of the snapshot.
func sliceSavedRunes(text string, start, end int) (string, bool) {
	if start < 0 || end < start || end > len(text) {
		return "", false
	}
	var position, startByte int
	endByte := len(text)
	for offset := range text {
		if position == start {
			startByte = offset
		}
		if position == end {
			endByte = offset
			break
		}
		position++
	}
	if end != position {
		return "", false
	}
	if start == end {
		return "", true
	}
	text = text[startByte:endByte]
	if !utf8.ValidString(text) {
		text = string([]rune(text))
	}
	return text, true
}

func resolveDeliveryEvidence(ctx context.Context, db *gorm.DB, requestID, id string, scope knowledge.RetrievalScope, documents map[[2]string]knowledge.Document) (string, StoredReference, error) {
	if !strings.HasPrefix(id, "ev_") {
		return "", StoredReference{}, fmt.Errorf("%q 不是片段句柄；调用 read_materials(kind='excerpts', queries=[...])，使用 matches[].evidence_id 原样填入，doc_id 和 observation_id 用于其他用途", id)
	}
	var window deliveryEvidence
	if err := db.WithContext(ctx).Where("request_id = ? AND id = ?", requestID, id).Take(&window).Error; err != nil {
		return "", StoredReference{}, fmt.Errorf("无法读取本任务的证据 %q: %w；从 excerpts 查询实际片段句柄", id, err)
	}
	ref := StoredReference{DocID: window.DocID, Revision: window.Revision}
	doc, err := resolveDeliveryDocument(ctx, knowledge.NewStore(db), [2]string{ref.DocID, ref.Revision}, scope, documents)
	if err != nil {
		return "", ref, err
	}
	text := doc.Body
	if window.Raw {
		text = doc.RawBody
	} else {
		ref.Start, ref.End = &window.Start, &window.End
	}
	excerpt, ok := sliceSavedRunes(text, window.Start, window.End)
	if !ok || window.Start == window.End {
		return "", ref, fmt.Errorf("证据 %q 的原文区间无效", id)
	}
	return excerpt, ref, nil
}
