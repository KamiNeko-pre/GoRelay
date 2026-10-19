package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

// Database fixtures exercise result processing, not autonomous source discovery.
func TestMonitorObservationsDeduplicateWithdrawAndRecover(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	m, err := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: "changes", Goal: spec.Goal, Spec: &spec, Conditions: []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}, {Field: "unit", Operator: "eq", Value: json.RawMessage(`"CNY"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(key string, amount any, unit string, fresh bool, failed bool) agentdata.Run {
		t.Helper()
		r, e := s.RefreshMonitor(ctx, m.ID, key)
		if e != nil {
			t.Fatal(e)
		}
		if !failed {
			acquired := time.Now().UTC()
			if !fresh {
				acquired = acquired.Add(-2 * time.Hour)
			}
			rows := []Record{{Key: "acquired-" + key, SchemaVersion: spec.SchemaVersion, Fields: map[string]any{"id": "a", "amount": amount, "unit": unit, "labels": []string{"zh"}}, Provenance: Provenance{SourceID: "public", URL: "https://example.com/data", FetchedAt: acquired, DataVersion: key}}}
			child := task.CrawlTask{RequestKey: "fixture-" + key, Status: task.StatusSucceeded, URL: "https://example.com/data"}
			if e = db.Create(&child).Error; e != nil {
				t.Fatal(e)
			}
			rows[0].Provenance.TaskID = child.ID
			raw, _ := json.Marshal(AcquisitionResult{Records: rows})
			if e = db.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(raw)}).Error; e != nil {
				t.Fatal(e)
			}
			if e = db.Create(&agentdata.RunTask{RunID: r.ID, TaskID: child.ID, OperationID: child.ID}).Error; e != nil {
				t.Fatal(e)
			}
		}
		status := "partial"
		if failed {
			status = "failed"
		}
		if e = db.Model(&r).Updates(map[string]any{"status": status, "last_error": "fixture terminal"}).Error; e != nil {
			t.Fatal(e)
		}
		if e = s.ProcessMonitorCheck(ctx, r.ID); e != nil {
			t.Fatal(e)
		}
		if e = s.ProcessMonitorCheck(ctx, r.ID); e != nil {
			t.Fatal("replay", e)
		}
		return r
	}
	inbox := func() []MonitorInbox {
		t.Helper()
		out := []MonitorInbox{}
		if e := db.Order("id ASC").Find(&out).Error; e != nil {
			t.Fatal(e)
		}
		return out
	}
	check("first", 4500, "CNY", true, false)
	items := inbox()
	if len(items) != 1 || !items[0].Active || !items[0].Verified {
		t.Fatal(items)
	}
	check("same", 4500, "CNY", true, false)
	if len(inbox()) != 1 {
		t.Fatal("new evidence duplicated unchanged opportunity")
	}
	check("changed", 4000, "CNY", true, false)
	items = inbox()
	if len(items) != 1 || items[0].Status != "unread" {
		t.Fatal("unread item not coalesced", items)
	}
	check("failed", nil, "", true, true)
	items = inbox()
	if !items[0].Active || items[0].Verified {
		t.Fatal("failure withdrew opportunity or claimed fresh", items)
	}
	check("stale", 0, "CNY", false, false)
	if inbox()[0].Verified {
		t.Fatal("old facts considered current")
	}
	check("invalid", nil, "CNY", true, false)
	if inbox()[0].Verified {
		t.Fatal("missing amount treated as free")
	}
	check("currency", 4000, "USD", true, false)
	items = inbox()
	if items[0].Active || !items[0].Verified {
		t.Fatal("fresh nonmatch failed to withdraw", items)
	}
	var withdrawn map[string]any
	if e := json.Unmarshal(items[0].Fields, &withdrawn); e != nil || withdrawn["unit"] != "USD" {
		t.Fatal("withdrawal showed old matching facts", withdrawn, e)
	}
	check("returned", 0, "CNY", true, false)
	items = inbox()
	if len(items) != 2 || !items[1].Active {
		t.Fatal("new episode missing or free treated as missing", items)
	}
	if e := db.Model(&items[1]).Update("status", "read").Error; e != nil {
		t.Fatal(e)
	}
	check("after-read", 3500, "CNY", true, false)
	items = inbox()
	if len(items) != 3 || items[2].Status != "unread" || items[1].Active {
		t.Fatal("new change after read lost", items)
	}
	if e := db.Model(&items[2]).Update("status", "ignored").Error; e != nil {
		t.Fatal(e)
	}
	check("ignored-change", 3000, "CNY", true, false)
	items = inbox()
	if len(items) != 3 || items[2].Status != "ignored" {
		t.Fatal("ignored episode notified again", items)
	}
	current, e := s.Monitor(ctx, m.ID)
	if e != nil || current.ActiveRunID != "" || current.LastRunID == "" {
		t.Fatal("terminal check kept monitor busy", current, e)
	}
}

func TestMonitorNoConditionsMeansNotifyOnBusinessChange(t *testing.T) {
	spec := monitorSpec()
	now := time.Now().UTC()
	rows := []Record{{Key: "one", SchemaVersion: "v1", Fields: map[string]any{"id": "a", "amount": 0, "unit": "CNY", "labels": []string{}}, Provenance: Provenance{SourceID: "public", URL: "https://example.com", FetchedAt: now, DataVersion: "v1", TaskID: 1}}}
	merged, e := MergeRecords(rows, spec.JoinKeys)
	if e != nil {
		t.Fatal(e)
	}
	valid, issues, e := monitorValidatedRows(spec, TaskResult{Records: merged.Records}, now)
	if e != nil || len(valid) != 1 || len(issues) != 0 {
		t.Fatal(valid, issues, e)
	}
	// Duplicate identities (usually conflicting sources) may not overwrite each other.
	duplicate := append(merged.Records, merged.Records[0])
	valid, issues, e = monitorValidatedRows(spec, TaskResult{Records: duplicate}, now)
	if e != nil || len(valid) != 0 || len(issues) == 0 {
		t.Fatal(valid, issues, e)
	}
	sibling := TaskSpec{Goal: "events", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"event", "string"}, {"seats", "integer"}}, JoinKeys: []string{"event"}, EntityField: "event", Entities: []string{"concert"}, MaxAgeSeconds: 60}
	rows[0].Fields = map[string]any{"event": "concert", "seats": 2}
	merged, e = MergeRecords(rows, sibling.JoinKeys)
	if e != nil {
		t.Fatal(e)
	}
	valid, _, e = monitorValidatedRows(sibling, TaskResult{Records: merged.Records}, now)
	if e != nil || len(valid) != 1 {
		t.Fatal("business-specific evaluator", valid, e)
	}
}

func TestMonitorExpiryUsesValidatedFieldEvidence(t *testing.T) {
	spec := monitorSpec()
	now := time.Now().UTC()
	valid := Provenance{SourceID: "public", URL: "https://example.com", TaskID: 1, DataVersion: "one", FetchedAt: now.Add(-30 * time.Minute)}
	invalid := valid
	invalid.TaskID = 0
	invalid.FetchedAt = now
	row := MergedRecord{SchemaVersion: "v1", Keys: []string{"key"}, Fields: map[string]any{"id": "a", "amount": 1, "unit": "CNY", "labels": []string{}}, FieldSources: map[string][]Provenance{}}
	for _, f := range spec.RequiredFields {
		row.FieldSources[f.Name] = []Provenance{valid, invalid}
	}
	until := monitorValidUntil(spec, row, now)
	if until == nil || !until.Equal(valid.FetchedAt.Add(time.Hour)) {
		t.Fatal("invalid newer evidence extended lifetime", until)
	}
	item := MonitorInbox{Verified: true, ValidUntil: until}
	if !item.Current(now) || item.Current(now.Add(time.Hour)) {
		t.Fatal("read-time expiry ignored")
	}
}
