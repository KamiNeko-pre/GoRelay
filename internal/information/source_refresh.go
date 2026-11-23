package information

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/mcpserver"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type sourceRefreshInput struct {
	PreviousJobID string `json:"previous_job_id"`
	RequestKey    string `json:"request_key"`
}

type sourceRefreshResult struct {
	Refresh agentdata.SourceRefresh `json:"refresh"`
	Job     mcpserver.SourceJob     `json:"job"`
}

func (b sourceBackend) checkRefresh(ctx context.Context, in *mcpserver.CollectInput) error {
	if in.RefreshID == "" {
		return nil
	}
	if _, err := b.store(); err != nil {
		return err
	}
	var cycle agentdata.SourceRefresh
	if err := b.service.DB.WithContext(ctx).First(&cycle, "id = ?", in.RefreshID).Error; err != nil {
		return fmt.Errorf("refresh_id does not identify a saved update request: %w; open the source and choose 更新一次", err)
	}
	if cycle.SourceID != in.SourceID || (in.WorkspaceID != "" && cycle.WorkspaceID != in.WorkspaceID) {
		return fmt.Errorf("refresh_id belongs to a different source or workspace; reuse the source and workspace returned for this update")
	}
	in.WorkspaceID = cycle.WorkspaceID
	return nil
}

func (b sourceBackend) refreshSource(ctx context.Context, in sourceRefreshInput) (out sourceRefreshResult, cause error) {
	if _, err := b.crawler(); err != nil {
		return out, err
	}
	if in.PreviousJobID == "" || in.RequestKey == "" || len(in.RequestKey) > 128 {
		return out, fmt.Errorf("previous_job_id and request_key (1..128 bytes) are required; update an existing source result")
	}
	previous, err := b.GetJob(ctx, mcpserver.JobInput{JobID: in.PreviousJobID})
	if err != nil {
		return out, err
	}
	if !previous.Terminal {
		out.Job = previous
		return out, nil
	}
	if previous.SourceID == "" || previous.URL == "" {
		return out, fmt.Errorf("the selected job has no reusable acquisition input; select a source acquisition result, not a source-building workspace")
	}
	if previous.WorkspaceID == "" {
		if previous.Goal == "" {
			previous.Goal = "更新已保存来源的数据"
		}
		workspace, err := b.service.Crawler.OpenSourceWorkspace(ctx, previous.Goal, nil)
		if err != nil {
			return out, err
		}
		previous.WorkspaceID = workspace.ID
	}
	params := previous.Params
	if params == nil {
		params = map[string]any{}
	}
	collect := mcpserver.CollectInput{WorkspaceID: previous.WorkspaceID, Goal: previous.Goal, SourceID: previous.SourceID, URL: previous.URL, Params: params}
	inputJSON := string(mustMarshal(collect))
	cause = b.service.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize updates of this saved result, not all source execution.
		if strings.HasPrefix(in.PreviousJobID, savedJobPrefix) {
			id, err := savedJobTaskID(in.PreviousJobID)
			if err != nil {
				return err
			}
			var parent task.CrawlTask
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ?", id).Error; err != nil {
				return err
			}
		} else if strings.HasPrefix(in.PreviousJobID, nativeJobPrefix) {
			var parent Request
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ?", strings.TrimPrefix(in.PreviousJobID, nativeJobPrefix)).Error; err != nil {
				return err
			}
		} else {
			var parent agentdata.Run
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ?", in.PreviousJobID).Error; err != nil {
				return err
			}
		}
		scoped := *b.service
		scoped.DB = tx
		crawler := *b.service.Crawler
		crawler.Data = agentdata.NewStore(tx)
		scoped.Crawler = &crawler
		backend := sourceBackend{service: &scoped}
		var cycle agentdata.SourceRefresh
		err := tx.Where("request_key = ?", in.RequestKey).First(&cycle).Error
		if err == nil {
			if cycle.PreviousJobID != in.PreviousJobID {
				return fmt.Errorf("request_key already identifies a different update request")
			}
			out.Refresh = cycle
			out.Job, err = backend.GetJob(ctx, mcpserver.JobInput{JobID: cycle.JobID})
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var recent []agentdata.SourceRefresh
		if err := tx.Where("previous_job_id = ? AND input_json = ?", in.PreviousJobID, inputJSON).Order("created_at DESC").Find(&recent).Error; err != nil {
			return err
		}
		for _, prior := range recent {
			job, err := backend.GetJob(ctx, mcpserver.JobInput{JobID: prior.JobID})
			if err != nil {
				return err
			}
			if !job.Terminal {
				out.Refresh, out.Job = prior, job
				return nil
			}
		}
		cycle = agentdata.SourceRefresh{ID: agentdata.Token(), RequestKey: in.RequestKey, SourceID: collect.SourceID, WorkspaceID: collect.WorkspaceID, PreviousJobID: in.PreviousJobID, InputJSON: inputJSON}
		if err := tx.Create(&cycle).Error; err != nil {
			return err
		}
		collect.RefreshID = cycle.ID
		job, err := backend.Collect(ctx, collect)
		if err != nil {
			return err
		}
		if err := tx.Model(&cycle).Update("job_id", job.JobID).Error; err != nil {
			return err
		}
		job.RefreshID, job.PreviousJobID = cycle.ID, cycle.PreviousJobID
		out.Refresh, out.Job = cycle, job
		return nil
	})
	return out, cause
}

func (b sourceBackend) decorateSourceJob(ctx context.Context, job *mcpserver.SourceJob) error {
	var cycle agentdata.SourceRefresh
	err := b.service.DB.WithContext(ctx).Where("job_id = ?", job.JobID).First(&cycle).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	job.RefreshID, job.PreviousJobID = cycle.ID, cycle.PreviousJobID
	return nil
}
