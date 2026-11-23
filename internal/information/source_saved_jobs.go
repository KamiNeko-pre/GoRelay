package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
)

const savedJobPrefix = "saved_"

func savedJobTaskID(id string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimPrefix(id, savedJobPrefix), 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid saved acquisition job_id")
	}
	return value, nil
}

// Web-created acquisitions already have durable tasks and results. Expose
// those same observations instead of requiring a second MCP collection.
func (b sourceBackend) savedSourceJob(ctx context.Context, id string) (out mcpserver.SourceJob, err error) {
	out.JobID = id
	if _, err = b.store(); err != nil {
		return out, err
	}
	taskID, err := savedJobTaskID(id)
	if err != nil {
		return out, err
	}
	var execution task.CrawlTask
	if err = b.service.DB.WithContext(ctx).First(&execution, "id = ?", taskID).Error; err != nil {
		return out, err
	}
	var saved repository.CrawlResult
	if err = b.service.DB.WithContext(ctx).First(&saved, "task_id = ?", taskID).Error; err != nil {
		return out, err
	}
	var result aiapp.AcquisitionResult
	if err = json.Unmarshal([]byte(saved.Payload), &result); err != nil {
		return out, err
	}
	if len(result.Records) == 0 {
		return out, fmt.Errorf("saved acquisition contains no source records")
	}
	var input struct {
		Params map[string]any  `json:"params"`
		Spec   *aiapp.TaskSpec `json:"spec"`
	}
	if execution.ProgramParams != "" {
		if err = json.Unmarshal([]byte(execution.ProgramParams), &input); err != nil {
			return out, err
		}
	}
	out.SourceID = result.Records[0].Provenance.SourceID
	out.URL, out.Params, out.Version = execution.URL, input.Params, execution.ProgramHash
	out.Status, out.Terminal, out.RecordsSaved = string(execution.Status), true, len(result.Records)
	out.CreatedAt, out.UpdatedAt = execution.CreatedAt, execution.UpdatedAt
	if input.Spec != nil {
		out.Goal, out.JoinKeys = input.Spec.Goal, input.Spec.JoinKeys
	}
	out.Tasks = []mcpserver.SourceTask{{TaskID: taskID, Status: out.Status, RecordsSaved: out.RecordsSaved}}
	return out, nil
}
