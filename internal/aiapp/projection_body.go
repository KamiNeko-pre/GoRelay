package aiapp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
)

// AcquisitionDocuments is shared by Lua, native API and Browser commits.
// The caller's declared dataset is independent of the capability implementation.
// Without that declaration, separate capability namespaces are not guessed equal.
func AcquisitionDocuments(records []Record, spec TaskSpec) ([]knowledge.Document, error) {
	documents := make([]knowledge.Document, 0, len(records))
	positions := map[string]int{}
	for _, record := range records {
		body, err := projectionBody(record, spec)
		if err != nil {
			return nil, err
		}
		title, _ := record.Fields["title"].(string)
		if title == "" {
			title = record.Key
		}
		origin, err := url.Parse(record.Provenance.URL)
		if err != nil || origin.Host == "" {
			return nil, fmt.Errorf("acquisition projection needs a valid origin URL")
		}
		sourceKey := ""
		if len(spec.JoinKeys) > 0 {
			keys := make([]any, 0, len(spec.JoinKeys))
			for _, key := range spec.JoinKeys {
				value, ok := record.Fields[key]
				if !ok || value == nil {
					return nil, fmt.Errorf("acquisition identity missing business key %s", key)
				}
				keys = append(keys, value)
			}
			sourceKey = agentdata.Hash(keys)
		} else {
			positions[record.Provenance.URL]++
			sourceKey = fmt.Sprintf("%s#row=%d", record.Provenance.URL, positions[record.Provenance.URL])
		}
		profile := make([]string, 0, len(record.Fields))
		for field := range businessFields(record, spec) {
			profile = append(profile, field)
		}
		sort.Strings(profile)
		projectionKey := agentdata.Hash([]any{sourceKey, strings.ToLower(origin.Host), record.SchemaVersion, profile})
		if len(spec.JoinKeys) == 0 {
			projectionKey = "weak-url-ordinal:" + projectionKey
		}
		namespace := ""
		if spec.SourceNamespace != "" {
			namespace = "acquisition:" + agentdata.Hash([]string{spec.SourceNamespace, strings.ToLower(origin.Host)})
		}
		doc, err := knowledge.NewDocument(knowledge.Input{IdentityNamespace: namespace, Source: record.Provenance.SourceID, SourceKey: projectionKey, URL: record.Provenance.URL, Title: title, Body: body, Format: "text", FetchedAt: record.Provenance.FetchedAt, PublishedAt: record.Provenance.PublishedAt, DataVersion: record.Provenance.DataVersion, TaskID: record.Provenance.TaskID, RecordKey: record.Key})
		if err != nil {
			return nil, err
		}
		documents = append(documents, doc)
	}
	return documents, nil
}

func projectionBody(record Record, spec TaskSpec) (string, error) {
	if body, ok := record.Fields["body"].(string); ok && body != "" {
		return body, nil
	}
	fields := businessFields(record, spec)
	raw, err := json.Marshal(fields)
	return string(raw), err
}
func businessFields(record Record, spec TaskSpec) map[string]any {
	fields := make(map[string]any, len(record.Fields))
	for key, value := range record.Fields {
		fields[key] = value
	}
	// source_url is the acquisition receipt binding marker (BindRecords), stored
	// separately in Head.URL/Provenance. It belongs in searchable facts only when
	// the frozen goal explicitly requests it, not merely because every row carries it.
	requested := false
	for _, field := range spec.RequiredFields {
		if field.Name == "source_url" {
			requested = true
			break
		}
	}
	if !requested {
		delete(fields, "source_url")
	}
	return fields
}
