package knowledge

import (
	"encoding/json"
)

// NewDocument 校验正文输入，并生成稳定身份与证据版本。
// 同一来源键保持 ID，正文或引用元数据变化则生成新 Revision。
func NewDocument(input Input) (Document, error) {
	prepared, err := NormalizeInput(input)
	if err != nil {
		return Document{}, err
	}

	namespace := prepared.Source
	if prepared.IdentityNamespace != "" {
		namespace = prepared.IdentityNamespace
	}
	id := digest("document", namespace, prepared.SourceKey)
	metadata, _ := json.Marshal([]any{prepared.FetchedAt, prepared.PublishedAt, prepared.DataVersion, prepared.TaskID, prepared.RecordKey})
	if prepared.IdentityNamespace != "" {
		metadata, _ = json.Marshal([]any{prepared.Source, prepared.FetchedAt, prepared.PublishedAt, prepared.DataVersion, prepared.TaskID, prepared.RecordKey})
	}
	revision := digest("revision", id, prepared.Title, prepared.URL, prepared.Format, prepared.Body, string(metadata))

	return Document{
		ID:        id,
		Revision:  revision,
		Source:    prepared.Source,
		SourceKey: prepared.SourceKey,
		URL:       prepared.URL,
		Title:     prepared.Title,
		Body:      prepared.Body,
		Format:    prepared.Format,
		FetchedAt: prepared.FetchedAt, PublishedAt: prepared.PublishedAt, DataVersion: prepared.DataVersion, TaskID: prepared.TaskID, RecordKey: prepared.RecordKey,
	}, nil
}
