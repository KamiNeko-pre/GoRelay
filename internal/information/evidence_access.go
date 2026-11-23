package information

import (
	"context"
	"fmt"
	"net/url"

	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
)

type sourceBackend struct{ service *Service }

func (b sourceBackend) store() (*knowledge.Store, error) {
	if b.service == nil || b.service.DB == nil {
		return nil, &mcpserver.ToolFailure{Code: "SERVICE_UNAVAILABLE", Message: "GoRelay's database is not initialized", RecoverySteps: []string{"Connect to the running GoRelay service with its existing database configuration.", "No model API key or vector service is needed for saved-evidence access."}}
	}
	return knowledge.NewStore(b.service.DB), nil
}

func citedEvidence(ctx context.Context, snapshot knowledge.EvidenceSnapshot) mcpserver.CitedEvidence {
	return mcpserver.CitedEvidence{EvidenceSnapshot: snapshot, SnapshotURL: mcpserver.RequestBaseURL(ctx) + "/evidence#id=" + url.QueryEscape(snapshot.ID)}
}

func (b sourceBackend) SearchEvidence(ctx context.Context, in mcpserver.EvidenceInput) (mcpserver.EvidenceResult, error) {
	out := mcpserver.EvidenceResult{Entries: []mcpserver.CitedEvidence{}}
	store, err := b.store()
	if err != nil {
		return out, err
	}
	page, err := store.SearchEvidence(ctx, knowledge.EvidenceQuery{Query: in.Query, Scope: knowledge.RetrievalScope{Sources: in.SourceIDs, DocumentIDs: in.DocumentIDs}, Cursor: in.Cursor, PageSize: in.PageSize})
	if err != nil {
		return out, err
	}
	out.Total, out.NextCursor, out.CorpusRevision = page.Total, page.NextCursor, page.CorpusRevision
	for _, entry := range page.Entries {
		out.Entries = append(out.Entries, citedEvidence(ctx, entry))
	}
	return out, nil
}

func (b sourceBackend) ReadEvidence(ctx context.Context, in mcpserver.EvidenceIDs) (mcpserver.EvidenceResult, error) {
	out := mcpserver.EvidenceResult{Entries: []mcpserver.CitedEvidence{}}
	if (len(in.IDs) > 0) == (in.DocumentID != "" || in.Revision != "") {
		return out, businessError("INVALID_EVIDENCE_QUERY", "facts", "请提供 evidence_ids，或者同时提供 document_id 与 revision", "查已有观测使用 kind='observations'，或传入要检索的关键词 query")
	}
	store, err := b.store()
	if err != nil {
		return out, err
	}
	if in.DocumentID != "" || in.Revision != "" {
		page, err := store.ReadEvidenceDocument(ctx, in.DocumentID, in.Revision, in.Cursor, in.PageSize)
		if err != nil {
			return out, err
		}
		out.Total, out.NextCursor, out.CorpusRevision = page.Total, page.NextCursor, page.CorpusRevision
		for _, entry := range page.Entries {
			out.Entries = append(out.Entries, citedEvidence(ctx, entry))
		}
		return out, nil
	}

	for _, id := range in.IDs {
		entry, err := store.ReadEvidenceSnapshot(ctx, id)
		if err != nil {
			return out, businessError("REFERENCE_UNAVAILABLE", "facts", fmt.Sprintf("read evidence %q: %v", id, err), "查询已保存资料(kind='documents' 或 kind='excerpt')获取有效引用版本后重试")
		}
		out.Entries = append(out.Entries, citedEvidence(ctx, entry))
		out.Total = len(out.Entries)
	}
	out.Total = len(out.Entries)
	return out, nil
}
