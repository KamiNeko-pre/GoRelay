package knowledge

import (
	"context"
	"fmt"
)

// DocumentReference uses the same rune offsets as saved evidence passages.
// Omitting both offsets refers to the whole immutable document revision.
type DocumentReference struct {
	DocID    string `json:"doc_id"`
	Revision string `json:"revision"`
	Field    string `json:"field,omitempty"`
	Start    *int   `json:"start,omitempty"`
	End      *int   `json:"end,omitempty"`
}

func (s *Store) ResolveReference(ctx context.Context, ref DocumentReference, scope RetrievalScope) (Document, error) {
	doc, err := s.ResolveDocument(ctx, ref.DocID, ref.Revision, scope)
	if err != nil {
		return doc, err
	}
	if err := checkReferenceOffsets(ref, len([]rune(doc.Body))); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func checkReferenceOffsets(ref DocumentReference, bodyRunes int) error {
	if (ref.Start == nil) != (ref.End == nil) {
		return fmt.Errorf("%w: start and end must be supplied together", ErrInvalidReference)
	}
	if ref.Start != nil && (*ref.Start < 0 || *ref.End < *ref.Start || *ref.End > bodyRunes) {
		return fmt.Errorf("%w: passage offsets are outside this document revision", ErrInvalidReference)
	}
	return nil
}

// ValidateReferences checks each exact version without fetching its body into Go.
func (s *Store) ValidateReferences(ctx context.Context, refs []DocumentReference, scope RetrievalScope) error {
	if len(refs) == 0 {
		return nil
	}
	type versionKey struct{ id, revision string }
	pairs := make([][]string, 0, len(refs))
	seen := map[versionKey]bool{}
	for _, ref := range refs {
		key := versionKey{ref.DocID, ref.Revision}
		if !seen[key] {
			pairs = append(pairs, []string{ref.DocID, ref.Revision})
			seen[key] = true
		}
	}
	type versionRow struct {
		ID, Revision, Source, URL string
		Active                    bool
		SavedID, SavedRevision    string
		BodyRunes                 int
	}
	var rows []versionRow
	err := s.DB.WithContext(ctx).Table("document_versions v").
		Select(`h.id, v.revision, h.source, h.url, h.active,
JSON_UNQUOTE(JSON_EXTRACT(v.json, '$.id')) AS saved_id,
JSON_UNQUOTE(JSON_EXTRACT(v.json, '$.revision')) AS saved_revision,
COALESCE(CHAR_LENGTH(JSON_UNQUOTE(JSON_EXTRACT(v.json, '$.body'))), 0) AS body_runes`).
		Joins("JOIN knowledge_heads h ON h.id = v.doc_id").
		Where("(v.doc_id, v.revision) IN ?", pairs).Scan(&rows).Error
	if err != nil {
		return err
	}
	versions := make(map[versionKey]versionRow, len(rows))
	for _, row := range rows {
		versions[versionKey{row.ID, row.Revision}] = row
	}
	for _, ref := range refs {
		row, ok := versions[versionKey{ref.DocID, ref.Revision}]
		if !ok || !row.Active || !scope.accepts(Head{ID: row.ID, Source: row.Source, URL: row.URL}) {
			return fmt.Errorf("%w: document %s revision %s is unavailable", ErrInvalidReference, ref.DocID, ref.Revision)
		}
		if row.SavedID != ref.DocID || row.SavedRevision != ref.Revision {
			return fmt.Errorf("%w: document version identity mismatch", ErrInvalidReference)
		}
		if err := checkReferenceOffsets(ref, row.BodyRunes); err != nil {
			return fmt.Errorf("document %s: %w", ref.DocID, err)
		}
	}
	return nil
}

// ResolveDocument checks access against the live head, then reads the exact
// immutable revision. A newer head does not revoke a historical reference.
func (s *Store) ResolveDocument(ctx context.Context, id, revision string, scope RetrievalScope) (Document, error) {
	if id == "" || revision == "" {
		return Document{}, fmt.Errorf("%w: document ID and exact revision required", ErrInvalidReference)
	}
	head, err := s.Head(ctx, id)
	if err != nil {
		return Document{}, err
	}
	if !head.Active || !scope.accepts(head) {
		return Document{}, fmt.Errorf("%w: document revoked or outside scope", ErrInvalidReference)
	}
	doc, err := s.Document(ctx, id, revision)
	if err != nil {
		return Document{}, err
	}
	if doc.ID != id || doc.Revision != revision {
		return Document{}, fmt.Errorf("%w: document version identity mismatch", ErrInvalidReference)
	}
	return doc, nil
}
