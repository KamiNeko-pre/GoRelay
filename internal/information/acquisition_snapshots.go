package information

import (
	"context"
	"encoding/json"
	"net/http"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/knowledge"
)

type acquisitionSnapshots struct {
	responses map[string]acquisition.Response
	documents map[string]knowledge.Document
}

// A new request, including an update of a prior delivery, starts with its own captures.
func (h *informationHost) acquisitionSnapshots(ctx context.Context) (acquisitionSnapshots, error) {
	return h.service.taskAcquisitionSnapshots(ctx, h.request.ID, h.input.SourceScope)
}

func (s *Service) taskAcquisitionSnapshots(ctx context.Context, requestID string, scope []string) (acquisitionSnapshots, error) {
	return s.loadAcquisitionSnapshots(ctx, requestID, scope, nil)
}

func (h *informationHost) snapshotsForRequests(ctx context.Context, fetcher *acquisition.PublicFetcher, options []acquisition.RequestOptions) (acquisitionSnapshots, error) {
	keys := []string{}
	for _, opts := range options {
		keys = append(keys, fetcher.SnapshotCandidates(ctx, opts)...)
	}
	return h.service.loadAcquisitionSnapshots(ctx, h.request.ID, h.input.SourceScope, keys)
}

// A nil filter supports Lua and other collectors with runtime-generated URLs.
func (s *Service) loadAcquisitionSnapshots(ctx context.Context, requestID string, scope, keys []string) (acquisitionSnapshots, error) {
	snapshots := acquisitionSnapshots{responses: map[string]acquisition.Response{}, documents: map[string]knowledge.Document{}}
	if keys != nil && len(keys) == 0 {
		return snapshots, nil
	}
	var links []taskDocument
	query := s.DB.WithContext(ctx).
		Where("request_id = ? AND status IN ? AND body_valid = ? AND snapshot_key <> ''", requestID, []string{"succeeded", reusedResponseStatus}, true)
	if keys != nil {
		query = query.Where("snapshot_key IN ?", keys)
	}
	err := query.Order("receipt_id").Find(&links).Error
	if err != nil {
		return acquisitionSnapshots{}, err
	}
	store := knowledge.NewStore(s.DB)
	for _, link := range links {
		if _, exists := snapshots.documents[link.SnapshotKey]; exists {
			continue
		}
		doc, err := store.ResolveDocument(ctx, link.DocID, link.Revision, knowledge.RetrievalScope{URLHosts: scope})
		if err != nil {
			return acquisitionSnapshots{}, err
		}
		body := doc.RawBody
		if body == "" {
			body = doc.Body // NewDocument elides the raw body only when it is identical.
		}
		if body == "" {
			continue
		}
		var headers map[string]string
		_ = json.Unmarshal([]byte(link.SnapshotHeaders), &headers)
		snapshots.responses[link.SnapshotKey] = acquisition.Response{Body: body, Receipt: acquisition.Receipt{
			Method: http.MethodGet, URL: doc.URL, RequestedURL: doc.URL, Status: link.HTTPStatus,
			FetchedAt: doc.FetchedAt, Headers: headers, RequestProfile: link.RequestProfile, SnapshotKey: link.SnapshotKey,
		}}
		snapshots.documents[link.SnapshotKey] = doc
	}
	return snapshots, nil
}

func (s acquisitionSnapshots) bind(resp acquisition.Response, positions map[int]knowledge.Document, i int) bool {
	if !resp.Receipt.ReusedSnapshot {
		return false
	}
	doc, ok := s.documents[resp.Receipt.SnapshotKey]
	if ok {
		positions[i] = doc
	}
	return ok
}
