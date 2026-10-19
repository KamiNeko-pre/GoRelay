// Package agentdata owns durable Agent facts, not AI action selection.
package agentdata

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

var ErrConflict = errors.New("agent state/version conflict")

func Token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Hash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type Run struct {
	ID                     string     `json:"id" gorm:"primaryKey;size:32"`
	RequestKey             string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	Goal                   string     `json:"goal" gorm:"type:text"`
	InputJSON              string     `json:"input_json" gorm:"type:longtext"`
	SpecJSON               string     `json:"spec_json" gorm:"type:longtext"`
	ContractProposalJSON   string     `json:"contract_proposal_json,omitempty" gorm:"type:longtext"`
	ContractAcceptanceJSON string     `json:"contract_acceptance_json,omitempty" gorm:"type:longtext"`
	PlanJSON               string     `json:"plan_json" gorm:"type:longtext"`
	ProgressJSON           string     `json:"progress_json" gorm:"type:longtext"`
	HostProgressJSON       string     `json:"host_progress_json,omitempty" gorm:"type:longtext"`
	NextWakeAt             *time.Time `json:"next_wake_at,omitempty" gorm:"index"`
	Status                 string     `json:"status" gorm:"size:32;index"`
	Token                  string     `json:"-" gorm:"size:32"`
	LeaseUntil             *time.Time `json:"lease_until,omitempty" gorm:"index"`
	Deadline               time.Time  `json:"deadline" gorm:"default:null"` // SQL NULL is represented as zero time.
	Cancelled              bool       `json:"cancelled"`
	BudgetJSON             string     `json:"budget_json" gorm:"type:longtext"`
	ResultJSON             string     `json:"result_json" gorm:"type:longtext"`
	LastError              string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

func (Run) TableName() string            { return "agent_runs" }
func (r Run) IsTerminal() bool           { return terminal(r.Status) }
func (r Run) Expired(now time.Time) bool { return !r.Deadline.IsZero() && !r.Deadline.After(now) }
func (r Run) RequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.Deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, r.Deadline)
}
func (r Run) MarshalJSON() ([]byte, error) {
	type plain Run
	var deadline *time.Time
	if !r.Deadline.IsZero() {
		deadline = &r.Deadline
	}
	return json.Marshal(struct {
		plain
		Deadline *time.Time `json:"deadline"`
	}{plain: plain(r), Deadline: deadline})
}

type Operation struct {
	ID            uint64    `json:"id" gorm:"primaryKey"`
	RunID         string    `json:"run_id" gorm:"size:32;uniqueIndex:operation_key"`
	Key           string    `json:"key" gorm:"size:128;uniqueIndex:operation_key"`
	InputHash     string    `json:"input_hash" gorm:"size:64"`
	Kind          string    `json:"kind" gorm:"size:64"`
	Stage         string    `json:"stage" gorm:"size:32"`
	ResultJSON    string    `json:"result_json" gorm:"type:longtext"`
	UsageJSON     string    `json:"usage_json" gorm:"type:text"`
	CostJSON      string    `json:"cost_reservation_json,omitempty" gorm:"type:text"`
	ResourcesJSON string    `json:"resource_reservation_json,omitempty" gorm:"type:text"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type RunTask struct {
	ID               uint64    `json:"id" gorm:"primaryKey"`
	RunID            string    `json:"run_id" gorm:"size:32;index"`
	OperationID      uint64    `json:"operation_id" gorm:"uniqueIndex"`
	TaskID           uint64    `json:"task_id" gorm:"uniqueIndex"`
	SourceID         string    `json:"source_id,omitempty" gorm:"size:64"`
	ArtifactHash     string    `json:"artifact_hash" gorm:"size:64"`
	RequirementsJSON string    `json:"requirements_json" gorm:"type:text"`
	CreatedAt        time.Time `json:"created_at"`
}
type ProgramArtifact struct {
	Hash             string    `json:"hash" gorm:"primaryKey;size:64"`
	Code             string    `json:"code" gorm:"type:longtext"`
	Kind             string    `json:"kind" gorm:"size:32"`
	PolicyVersion    string    `json:"policy_version" gorm:"size:64"`
	OutputSchema     string    `json:"output_schema" gorm:"type:text"`
	InputSchema      string    `json:"input_schema" gorm:"type:text"`
	InputExampleJSON string    `json:"input_example_json" gorm:"type:text"`
	ScopeJSON        string    `json:"scope_json" gorm:"type:text"`
	CreatedAt        time.Time `json:"created_at"`
}
type SourceHead struct {
	SourceID     string `json:"source_id" gorm:"primaryKey;size:64"`
	ArtifactHash string `json:"artifact_hash" gorm:"size:64"`
	Epoch        uint64 `json:"epoch"`
	Status       string `json:"status" gorm:"size:32"`
	Reason       string `json:"reason,omitempty" gorm:"type:text"`
}
type SourceVersion struct {
	ID               uint64    `json:"id" gorm:"primaryKey"`
	SourceID         string    `json:"source_id" gorm:"size:64;uniqueIndex:source_version"`
	ArtifactHash     string    `json:"artifact_hash" gorm:"size:64;uniqueIndex:source_version"`
	Description      string    `json:"description" gorm:"type:text"`
	ValidatorVersion string    `json:"validator_version" gorm:"size:64"`
	ProofJSON        string    `json:"proof_json" gorm:"type:longtext"`
	Status           string    `json:"status" gorm:"size:32"`
	CreatedAt        time.Time `json:"created_at"`
}
type Trace struct {
	ID        uint64    `json:"id" gorm:"primaryKey"`
	RunID     string    `json:"run_id" gorm:"size:32;index"`
	Stage     string    `json:"stage" gorm:"size:64"`
	DataJSON  string    `json:"data_json" gorm:"type:longtext"`
	CreatedAt time.Time `json:"created_at"`
}
type Store struct{ DB *gorm.DB }

func (s *Store) WithOwnedRun(ctx context.Context, id, token string, fn func(*Store) error) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := owned(tx, id, token); err != nil {
			return err
		}
		return fn(NewStore(tx))
	})
}

func NewStore(db *gorm.DB) *Store { return &Store{db} }
func (s *Store) Migrate() error {
	return s.DB.AutoMigrate(&Run{}, &Operation{}, &RunTask{}, &ProgramArtifact{}, &SourceHead{}, &SourceVersion{}, &Trace{}, &DailyAdmission{}, &task.CrawlTask{}, &task.TaskOutbox{})
}
func terminal(status string) bool {
	switch status {
	case "completed", "partial", "failed", "cancelled", "expired":
		return true
	}
	return false
}
func (s *Store) CreateRun(ctx context.Context, key, goal, input, budget string, deadline time.Time) (Run, error) {
	spec, err := submittedContract(input)
	if err != nil {
		return Run{}, err
	}
	r := Run{ID: Token(), RequestKey: key, Goal: goal, InputJSON: input, SpecJSON: spec, Status: "planning", BudgetJSON: budget, Deadline: deadline}
	if spec != "" {
		r.ContractAcceptanceJSON = ContractAcceptanceJSON(spec, "caller", input, time.Now())
	}
	if key == "" || goal == "" {
		return Run{}, fmt.Errorf("request key and goal required")
	}
	result := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
	if result.Error != nil {
		return Run{}, result.Error
	}
	if result.RowsAffected == 0 {
		if err := s.DB.WithContext(ctx).Where("request_key = ?", key).First(&r).Error; err != nil {
			return r, err
		}
		if r.Goal != goal || r.InputJSON != input {
			return r, ErrConflict
		}
	}
	return s.RestoreSubmittedContract(ctx, r.ID)
}
func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	var r Run
	err := s.DB.WithContext(ctx).First(&r, "id = ?", id).Error
	return r, err
}
func (s *Store) Claim(ctx context.Context, id string, ttl time.Duration) (Run, error) {
	return s.claimStatuses(ctx, id, ttl, []string{"planning", "running", "waiting", "reviewing", "blocked_core", "blocked_configuration", "needs_input"})
}

// ClaimAssetRun is reserved for the host that owns a published acquisition.
// The normal Agent scheduler still cannot claim or schedule asset_running.
func (s *Store) ClaimAssetRun(ctx context.Context, id string, ttl time.Duration) (Run, error) {
	return s.claimStatuses(ctx, id, ttl, []string{"asset_running"})
}

func (s *Store) claimStatuses(ctx context.Context, id string, ttl time.Duration, statuses []string) (Run, error) {
	now := time.Now()
	token := Token()
	until := now.Add(ttl)
	r, err := s.RestoreSubmittedContract(ctx, id)
	if err != nil {
		return r, err
	}
	if terminal(r.Status) || (r.SpecJSON == "" && r.ContractProposalJSON != "") {
		return r, ErrConflict
	}
	q := s.DB.WithContext(ctx).Model(&Run{}).Where("id = ? AND cancelled = ? AND status IN ? AND (contract_proposal_json IS NULL OR contract_proposal_json = '' OR spec_json <> '') AND (deadline IS NULL OR deadline > ?) AND (lease_until IS NULL OR lease_until <= ?)", id, false, statuses, now, now).Updates(map[string]any{"token": token, "lease_until": until})
	if q.Error != nil {
		return Run{}, q.Error
	}
	if q.RowsAffected != 1 {
		return Run{}, ErrConflict
	}
	return s.Run(ctx, id)
}
func owned(tx *gorm.DB, id, token string) (Run, error) {
	var r Run
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error
	if err != nil {
		return r, err
	}
	now := time.Now()
	if token == "" || r.Token != token || r.LeaseUntil == nil || !r.LeaseUntil.After(now) || r.Cancelled || r.Expired(now) || terminal(r.Status) {
		return r, ErrConflict
	}
	return r, nil
}

// Only a live owner can extend its lease. Renewal cannot resurrect an expired,
// cancelled or replaced owner, and never changes its durable token or budget.
func (s *Store) RenewRunLease(ctx context.Context, id, token string, ttl time.Duration) error {
	if token == "" || ttl <= 0 {
		return ErrConflict
	}
	now := time.Now()
	result := s.DB.WithContext(ctx).Model(&Run{}).
		Where("id = ? AND token = ? AND cancelled = ? AND lease_until > ? AND (deadline IS NULL OR deadline > ?) AND status NOT IN ?", id, token, false, now, now, []string{"completed", "partial", "failed", "cancelled", "expired"}).
		Update("lease_until", now.Add(ttl))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}
func (s *Store) SaveRun(ctx context.Context, id, token string, updates map[string]any) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := owned(tx, id, token); err != nil {
			return err
		}
		updates["lease_until"] = nil
		updates["token"] = ""
		return tx.Model(&Run{}).Where("id = ?", id).Updates(updates).Error
	})
}
func (s *Store) Cancel(ctx context.Context, id string) error {
	r, err := s.Run(ctx, id)
	if err != nil {
		return err
	}
	if terminal(r.Status) {
		return ErrConflict
	}
	q := s.DB.WithContext(ctx).Model(&Run{}).Where("id = ? AND status = ?", id, r.Status).Updates(map[string]any{"cancelled": true, "status": "cancelled", "token": "", "lease_until": nil})
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}
func (s *Store) Pending(ctx context.Context, limit int) ([]Run, error) {
	var rows []Run
	// Service owns expiry together with a readable result snapshot. Scheduling
	// must not silently terminalize a Run before that projection is saved.
	err := s.DB.WithContext(ctx).Where("status IN ? AND cancelled = ? AND (deadline IS NULL OR deadline > ?) AND (lease_until IS NULL OR lease_until <= ?) AND (next_wake_at IS NULL OR next_wake_at <= ?)", []string{"planning", "running", "waiting", "reviewing"}, false, time.Now(), time.Now(), time.Now()).Order("created_at").Limit(limit).Find(&rows).Error
	return rows, err
}
func (s *Store) Children(ctx context.Context, id string) ([]RunTask, error) {
	var rows []RunTask
	err := s.DB.WithContext(ctx).Where("run_id = ?", id).Order("id").Find(&rows).Error
	return rows, err
}
func (s *Store) Operations(ctx context.Context, id string) ([]Operation, error) {
	var rows []Operation
	err := s.DB.WithContext(ctx).Where("run_id = ?", id).Order("id").Find(&rows).Error
	return rows, err
}
func (s *Store) AddTrace(ctx context.Context, id, stage string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.DB.WithContext(ctx).Create(&Trace{RunID: id, Stage: stage, DataJSON: string(b)}).Error
}
func (s *Store) Traces(ctx context.Context, id string) ([]Trace, error) {
	var rows []Trace
	err := s.DB.WithContext(ctx).Where("run_id = ?", id).Order("id").Limit(200).Find(&rows).Error
	return rows, err
}
func saveArtifact(tx *gorm.DB, a ProgramArtifact) (ProgramArtifact, error) {
	identity := []string{a.Code, a.Kind, a.PolicyVersion, a.InputSchema, a.OutputSchema, a.ScopeJSON}
	// Preserve existing version IDs; examples are immutable only for new artifacts.
	if a.InputExampleJSON != "" {
		identity = append(identity, a.InputExampleJSON)
	}
	a.Hash = Hash(identity)
	q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&a)
	return a, q.Error
}
func (s *Store) SaveArtifact(ctx context.Context, a ProgramArtifact) (ProgramArtifact, error) {
	return saveArtifact(s.DB.WithContext(ctx), a)
}
func (s *Store) Artifact(ctx context.Context, hash string) (ProgramArtifact, error) {
	var a ProgramArtifact
	err := s.DB.WithContext(ctx).First(&a, "hash = ?", hash).Error
	return a, err
}

func (s *Store) ReserveOperation(ctx context.Context, id, token, key, inputHash, kind string, charge func(string) (string, error), compatibleHashes ...string) (Operation, bool, error) {
	return s.ReserveOperationWithSpend(ctx, id, token, key, inputHash, kind, charge, nil, compatibleHashes...)
}

func (s *Store) ReserveOperationWithSpend(ctx context.Context, id, token, key, inputHash, kind string, charge func(string) (string, error), quote *SpendQuote, compatibleHashes ...string) (Operation, bool, error) {
	return s.ReserveOperationWithResources(ctx, id, token, key, inputHash, kind, charge, quote, "", compatibleHashes...)
}

func (s *Store) ReserveOperationWithResources(ctx context.Context, id, token, key, inputHash, kind string, charge func(string) (string, error), quote *SpendQuote, resources string, compatibleHashes ...string) (Operation, bool, error) {
	var op Operation
	fresh := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, err := owned(tx, id, token)
		if err != nil {
			return err
		}
		err = tx.Where("run_id = ? AND `key` = ?", id, key).First(&op).Error
		if err == nil {
			matches := op.InputHash == inputHash
			for _, legacy := range compatibleHashes {
				matches = matches || op.InputHash == legacy
			}
			if !matches || op.Kind != kind {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if charge != nil {
			next, err := charge(r.BudgetJSON)
			if err != nil {
				return err
			}
			if err = tx.Model(&Run{}).Where("id = ?", id).Update("budget_json", next).Error; err != nil {
				return err
			}
			r.BudgetJSON = next
		}
		costJSON := ""
		if quote != nil {
			next, cost, e := reserveSpend(r.BudgetJSON, quote)
			if e != nil {
				return e
			}
			if e = tx.Model(&Run{}).Where("id = ?", id).Update("budget_json", next).Error; e != nil {
				return e
			}
			costJSON = cost
		}
		op = Operation{RunID: id, Key: key, InputHash: inputHash, Kind: kind, Stage: "reserved", CostJSON: costJSON, ResourcesJSON: resources}
		if err = tx.Create(&op).Error; err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return op, fresh, err
}
func (s *Store) CompleteOperation(ctx context.Context, id, token string, opID uint64, stage, result, usage string, settle ...func(string, Operation) (string, error)) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, err := owned(tx, id, token)
		if err != nil {
			return err
		}
		if len(settle) > 0 {
			var op Operation
			if err = tx.First(&op, "id = ? AND run_id = ? AND stage = ?", opID, id, "reserved").Error; err != nil {
				return err
			}
			next, err := settle[0](r.BudgetJSON, op)
			if err != nil {
				return err
			}
			if next != r.BudgetJSON {
				if err = tx.Model(&Run{}).Where("id = ?", id).Update("budget_json", next).Error; err != nil {
					return err
				}
			}
		}
		q := tx.Model(&Operation{}).Where("id = ? AND run_id = ? AND stage = ?", opID, id, "reserved").Updates(map[string]any{"stage": stage, "result_json": result, "usage_json": usage})
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return ErrConflict
		}
		return nil
	})
}
func (s *Store) SubmitChild(ctx context.Context, id, token, key, inputHash string, a ProgramArtifact, url, params string, beforeCommit func() error) (RunTask, error) {
	var link RunTask
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run, err := owned(tx, id, token)
		if err != nil {
			return err
		}
		var op Operation
		err = tx.Where("run_id = ? AND `key` = ?", id, key).First(&op).Error
		if err == nil {
			if op.InputHash != inputHash {
				return ErrConflict
			}
			return tx.First(&link, "operation_id = ?", op.ID).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		artifact, err := saveArtifact(tx, a)
		if err != nil {
			return err
		}
		var submitted struct {
			SourceID string          `json:"source_id"`
			Spec     json.RawMessage `json:"spec"`
		}
		if err = json.Unmarshal([]byte(params), &submitted); err != nil {
			return err
		}
		if submitted.SourceID != "" {
			var head SourceHead
			if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&head, "source_id = ?", submitted.SourceID).Error; err != nil {
				return err
			}
			if head.Status != "active" || head.ArtifactHash != artifact.Hash {
				return ErrConflict
			}
		}
		op = Operation{RunID: id, Key: key, InputHash: inputHash, Kind: "submit_acquisition", Stage: "succeeded"}
		if err = tx.Create(&op).Error; err != nil {
			return err
		}
		queuedAt := time.Now()
		child := task.CrawlTask{RequestKey: Hash([]string{id, key}), Target: "agent_program", URL: url, Status: task.StatusQueued, QueuedAt: &queuedAt, ProgramHash: artifact.Hash, ProgramParams: params, RunID: id}
		if !run.Deadline.IsZero() {
			deadline := run.Deadline
			child.ExecutionDeadline = &deadline
		}
		if err = tx.Create(&child).Error; err != nil {
			return err
		}
		if err = tx.Create(&task.TaskOutbox{TaskID: child.ID}).Error; err != nil {
			return err
		}
		link = RunTask{RunID: id, OperationID: op.ID, TaskID: child.ID, ArtifactHash: artifact.Hash, SourceID: submitted.SourceID, RequirementsJSON: string(submitted.Spec)}
		if err = tx.Create(&link).Error; err != nil {
			return err
		}
		if beforeCommit != nil {
			return beforeCommit()
		}
		return nil
	})
	return link, err
}
