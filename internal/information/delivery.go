package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultDeliveryTableTitle = "采集结果"

type Delivery struct {
	ID                   string    `json:"id" gorm:"primaryKey;size:32"`
	RequestID            string    `json:"request_id" gorm:"size:32;uniqueIndex:delivery_submission"`
	SubmissionReceiptID  uint64    `json:"submission_receipt_id" gorm:"uniqueIndex:delivery_submission"`
	RequirementsRevision uint64    `json:"requirements_revision"`
	PayloadJSON          string    `json:"-" gorm:"type:longtext"`
	CreatedAt            time.Time `json:"created_at"`
}

func (Delivery) TableName() string { return "information_deliveries" }

func (s *Service) SavedDelivery(ctx context.Context, id string) (Delivery, DeliveryDraft, error) {
	var row Delivery
	err := s.DB.WithContext(ctx).First(&row, "id = ?", id).Error
	var draft DeliveryDraft
	if err == nil {
		err = json.Unmarshal([]byte(row.PayloadJSON), &draft)
		for i := range draft.Tables {
			table := &draft.Tables[i]
			if len(table.RowObservationIDs) != len(table.Rows) {
				aligned := make([][]string, len(table.Rows))
				copy(aligned, table.RowObservationIDs)
				table.RowObservationIDs = aligned
			}
		}
	}
	return row, draft, err
}

func collectDeliveryObservationIDs(draft DeliveryDraft) []string {
	seen := map[string]bool{}
	selected := []string{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			selected = append(selected, id)
		}
	}
	for _, table := range draft.Tables {
		for _, ids := range table.RowObservationIDs {
			for _, id := range ids {
				add(id)
			}
		}
	}
	for _, deriv := range draft.RequirementResponses {
		for _, id := range deriv.ObservationIDs {
			add(id)
		}
	}
	return selected
}

func normalizeDeliveryDraft(requirements TaskRequirements, draft *DeliveryDraft) {
	draft.Outcome = normalizeDeliveryOutcome(draft.Outcome)
	usedTitles := map[string]bool{}
	for _, table := range draft.Tables {
		if table.Title != "" {
			usedTitles[table.Title] = true
		}
	}
	for i := range draft.Tables {
		table := &draft.Tables[i]
		if strings.TrimSpace(table.Title) == "" {
			name := defaultDeliveryTableTitle
			for n := 2; usedTitles[name]; n++ {
				name = fmt.Sprintf("%s %d", defaultDeliveryTableTitle, n)
			}
			table.Title, usedTitles[name] = name, true
		}
		if len(table.Columns) == 0 {
			columns := map[string]bool{}
			for _, row := range table.Rows {
				for column := range row {
					columns[column] = true
				}
			}
			for column := range columns {
				table.Columns = append(table.Columns, column)
			}
			sort.Strings(table.Columns)
		}
	}
	if draft.RequirementsRevision == 0 {
		draft.RequirementsRevision = requirements.Revision
		if draft.RequirementsRevision == 0 {
			draft.RequirementsRevision = 1
		}
	}
	if draft.Outcome == "partial" && len(draft.Gaps) == 0 {
		for _, response := range draft.RequirementResponses {
			if (response.Status == "unknown" || response.Status == "unmet") && strings.TrimSpace(response.Explanation) != "" {
				draft.Gaps = append(draft.Gaps, response.Explanation)
			}
		}
	}
}

// SubmitDelivery commits the result and terminal state together. Replays read
// the saved result before validating current requirements or reference access.
func (s *Service) SubmitDelivery(ctx context.Context, requestID string, receiptID uint64, draft DeliveryDraft) (Delivery, error) {
	if normalizeDeliveryOutcome(draft.Outcome) != "completed" || len(draft.Calculations) == 0 || s.auditProvider() == nil {
		return s.submitDelivery(ctx, requestID, receiptID, draft, nil, "")
	}
	// Formula rendering mutates table cell maps. Validate an independent copy
	// before calling the provider, then confirm the same packet at commit.
	var proposed DeliveryDraft
	if err := json.Unmarshal(mustMarshal(draft), &proposed); err != nil {
		return Delivery{}, err
	}
	var packet deliveryAuditInput
	result, err := s.submitDelivery(ctx, requestID, receiptID, proposed, &packet, "")
	if !errors.Is(err, errDeliveryAuditRequired) {
		return result, err
	}
	if err := s.runDeliveryAudit(ctx, requestID, receiptID, packet); err != nil {
		return Delivery{}, err
	}
	return s.submitDelivery(ctx, requestID, receiptID, draft, &packet, agentdata.Hash(packet))
}

func (s *Service) submitDelivery(ctx context.Context, requestID string, receiptID uint64, draft DeliveryDraft, audit *deliveryAuditInput, acceptedHash string) (Delivery, error) {
	var result Delivery
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		reqQuery := tx
		if audit == nil || acceptedHash != "" {
			reqQuery = reqQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := reqQuery.First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		err := tx.First(&result, "request_id = ? AND submission_receipt_id = ?", requestID, receiptID).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if request.CurrentDeliveryID != "" {
			return businessError("TASK_ALREADY_DELIVERED", "delivery", "该任务已有正式交付："+request.CurrentDeliveryID, "读取已有交付；再次更新须创建新任务并关联该交付")
		}
		if request.Status != "running" {
			return businessError("TASK_NOT_RUNNING", "delivery", "任务已终止或正在等待，不能提交", "由统一恢复入口接续当前任务；不要绕过取消或等待状态")
		}
		var receipt Step
		if err := tx.First(&receipt, "id = ? AND request_id = ?", receiptID, requestID).Error; err != nil {
			return err
		}
		if err := requireNativeCommitOwner(tx, ctx, request, receipt); err != nil {
			return err
		}
		var requirements TaskRequirements
		if err := json.Unmarshal([]byte(request.RequirementsJSON), &requirements); err != nil {
			return err
		}
		var input Input
		if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
			return err
		}
		normalizeDeliveryDraft(requirements, &draft)
		if draft.Outcome == "completed" && len(draft.Calculations) == 0 && request.Purpose != "source_discovery" {
			var plan WorkPlan
			if request.PlanJSON != "" {
				if err := json.Unmarshal([]byte(request.PlanJSON), &plan); err != nil {
					return err
				}
			}
			priorCalculation, err := hasPriorCalculationSubmission(tx, requestID)
			if err != nil {
				return err
			}
			if plan.NeedsCalculation || priorCalculation || taskRequiresCalculation(requirements) {
				return businessError("DELIVERY_CALCULATION_REQUIRED", "delivery", "当前任务的派生结果需要提交可复算公式", "保留已有证据，在 calculations 声明公式和原始输入；只有确实缺少影响结论的规则时，使用 partial 并指出缺口")
			}
		}
		store := knowledge.NewStore(tx)
		scope := knowledge.RetrievalScope{URLHosts: input.SourceScope}
		documents := map[[2]string]knowledge.Document{}
		decodedJSON := map[[2]string]any{}
		structuredEvidence := map[[3]string]map[string]string{}
		var values, sharedInputs map[string]float64
		if audit != nil {
			values = map[string]float64{}
			if draft.CalculationInputs != nil {
				sharedInputs = map[string]float64{}
			}
		}
		if err := verifyDeliveryCalculationsWithSources(&draft, func(ref StoredReference) ([]string, error) {
			doc, err := resolveDeliveryDocument(ctx, store, [2]string{ref.DocID, ref.Revision}, scope, documents)
			if err != nil {
				return nil, err
			}
			if (ref.Start == nil) != (ref.End == nil) {
				return nil, fmt.Errorf("%w: start and end must be supplied together", knowledge.ErrInvalidReference)
			}
			if ref.Start != nil {
				excerpt, ok := sliceSavedRunes(doc.Body, *ref.Start, *ref.End)
				if !ok {
					return nil, fmt.Errorf("%w: passage offsets are outside this document revision", knowledge.ErrInvalidReference)
				}
				return []string{excerpt}, nil
			}
			texts := []string{doc.Body}
			if doc.RawBody != "" && doc.RawBody != doc.Body {
				texts = append(texts, doc.RawBody)
			}
			return texts, nil
		}, calculationSources{
			prompt: input.Goal, results: values, sharedInputs: sharedInputs,
			evidence: func(id string) (string, StoredReference, error) {
				return resolveDeliveryEvidence(ctx, tx, requestID, id, scope, documents)
			},
			structured: func(location JSONCalculationEvidence) (float64, StoredReference, error) {
				value, ref, citation, err := resolveJSONDeliveryEvidence(ctx, tx, requestID, scope, location, documents, decodedJSON)
				if err == nil && audit != nil {
					structuredEvidence[[3]string{ref.DocID, ref.Revision, citation["json_pointer"]}] = citation
				}
				return value, ref, err
			},
		}); err != nil {
			return err
		}
		refs := append([]StoredReference{}, draft.References...)
		if len(draft.AcquisitionReceiptIDs) > 0 {
			batchRefs, err := bindDeliveryBatches(tx, requestID, &draft)
			if err != nil {
				return err
			}
			refs = append(refs, batchRefs...)
		}
		if request.Purpose == "source_discovery" {
			confirmed, err := confirmedDiscoveryReferences(tx, requestID, input.SourceScope, &draft)
			if err != nil {
				return err
			}
			refs = append(refs, confirmed...)
		}
		selected := collectDeliveryObservationIDs(draft)
		if len(selected) > 0 {
			var rows []observationRow
			if err := tx.Where("id IN ? AND id IN (?)", selected, tx.Model(&taskObservation{}).Select("observation_id").Where("request_id = ?", requestID)).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) != len(selected) {
				return businessError("OBSERVATION_NOT_LINKED", "delivery", "部分所选观测不属于当前任务", "用 read_materials 取得当前任务的观测 ID，再提交")
			}
			for _, row := range rows {
				fact, err := decodeObservation(row)
				if err != nil {
					return err
				}
				refs = append(refs, fact.References...)
			}
		}
		draft.References = nil
		evidence := DeliveryContext{BaselineDeliveryID: request.BaselineDeliveryID}
		seenRefs := map[string]bool{}
		for _, ref := range refs {
			key := string(mustMarshal(ref))
			if seenRefs[key] {
				continue
			}
			seenRefs[key] = true
			draft.References = append(draft.References, ref)
		}
		evidence.NewCaptures, err = validateDeliveryLinks(tx, requestID, draft.References)
		if err != nil {
			return err
		}
		if err := store.ValidateReferences(ctx, draft.References, scope); err != nil {
			return businessError("REFERENCE_UNAVAILABLE", "reference_resolver", err.Error(), "查询当前可访问资料，使用确切原文版本后重新提交")
		}
		if err := checkDelivery(requirements, draft, evidence); err != nil {
			return err
		}
		if audit != nil {
			scoped := *s
			scoped.DB = tx
			sources, err := scoped.deliveryAuditSources(ctx, requestID, input, draft, documents, structuredEvidence)
			if err != nil {
				return err
			}
			*audit = deliveryAuditInput{Goal: input.Goal, Spec: input.Spec, Requirements: requirements, Draft: draft, Values: values, SharedInputs: sharedInputs, Sources: sources}
			if draft.CalculationInputs != nil && len(draft.CalculationInputs.PromptIndices) > 0 {
				audit.PromptQuantities = parsedPromptQuantities(input.Goal)
			}
			if acceptedHash == "" {
				return errDeliveryAuditRequired
			}
			if agentdata.Hash(*audit) != acceptedHash {
				return businessError("DELIVERY_EVIDENCE_CHANGED", "delivery", "审计完成后相关任务要求或证据已变化", "读取最新证据并更新受影响的结论后提交")
			}
		}
		payload, err := json.Marshal(draft)
		if err != nil {
			return fmt.Errorf("encode delivery: %w", err)
		}
		result = Delivery{ID: agentdata.Token(), RequestID: requestID, SubmissionReceiptID: receiptID, RequirementsRevision: requirements.Revision, PayloadJSON: string(payload)}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		return tx.Model(&Request{}).Where("id = ?", requestID).Updates(map[string]any{"current_delivery_id": result.ID, "status": "completed", "stage": "completed"}).Error
	})
	return result, err
}

func validateDeliveryLinks(tx *gorm.DB, requestID string, refs []StoredReference) (int64, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	type versionKey struct{ id, revision string }
	type TaskDocument = taskDocument
	type linkedDocument struct {
		TaskDocument   `gorm:"embedded"`
		FreshReceiptID uint64
	}
	linked := map[versionKey][]linkedDocument{}
	pairs := make([][]string, 0, len(refs))
	for _, ref := range refs {
		key := versionKey{ref.DocID, ref.Revision}
		if _, seen := linked[key]; !seen {
			pairs = append(pairs, []string{ref.DocID, ref.Revision})
			linked[key] = nil
		}
	}
	var documents []linkedDocument
	err := tx.Table("information_task_documents d").Select("d.*, COALESCE(s.id, 0) AS fresh_receipt_id").
		Joins("LEFT JOIN information_steps s ON s.id = d.receipt_id AND s.request_id = d.request_id AND d.status = ?", "succeeded").
		Where("d.request_id = ? AND (d.doc_id, d.revision) IN ?", requestID, pairs).Scan(&documents).Error
	if err != nil {
		return 0, err
	}
	for _, doc := range documents {
		key := versionKey{doc.DocID, doc.Revision}
		linked[key] = append(linked[key], doc)
	}
	fresh := map[uint64]bool{}
	for _, ref := range refs {
		documents := linked[versionKey{ref.DocID, ref.Revision}]
		if len(documents) == 0 {
			return 0, businessError("REFERENCE_NOT_LINKED", "delivery", "原文未关联当前任务："+ref.DocID, "从任务资料查询取得引用，或显式关联有权读取的资料")
		}
		usable := false
		for _, doc := range documents {
			if isValidDocumentForCompleted(doc.TaskDocument) {
				usable = true
				if doc.FreshReceiptID != 0 {
					fresh[doc.FreshReceiptID] = true
				}
			}
		}
		if !usable {
			return 0, businessError("REFERENCE_NOT_USABLE", "delivery", "所选引用是失败或无内容的响应："+ref.DocID, "选择可用来源的原文；访问失败可写进 partial 的缺口")
		}
	}
	return int64(len(fresh)), nil
}

func bindDeliveryBatches(tx *gorm.DB, requestID string, draft *DeliveryDraft) ([]StoredReference, error) {
	var luaSteps []Step
	if err := tx.Select("id, input_json").Where("request_id = ? AND id IN ? AND kind = ?", requestID, draft.AcquisitionReceiptIDs, "run_lua").Find(&luaSteps).Error; err != nil {
		return nil, err
	}
	probes := map[uint64]bool{}
	for _, step := range luaSteps {
		var args struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal([]byte(step.InputJSON), &args) == nil && args.Mode == "probe" {
			probes[step.ID] = true
		}
	}
	var docs []taskDocument
	if err := tx.Where("request_id = ? AND receipt_id IN ?", requestID, draft.AcquisitionReceiptIDs).Order("receipt_id, response_key").Find(&docs).Error; err != nil {
		return nil, err
	}
	refs := make([]StoredReference, 0, len(docs))
	found := make(map[uint64]bool)
	for _, doc := range docs {
		if isValidDocumentForCompleted(doc) && !probes[doc.ReceiptID] {
			found[doc.ReceiptID] = true
			refs = append(refs, StoredReference{DocID: doc.DocID, Revision: doc.Revision})
		}
	}
	for _, id := range draft.AcquisitionReceiptIDs {
		if probes[id] && (len(found) > 0 || len(draft.References) > 0) {
			continue
		}
		if !found[id] {
			return nil, businessError("ACQUISITION_RECEIPT_NOT_USABLE", "delivery", fmt.Sprintf("回执 %d 没有本任务可交付的来源资料", id), "选择含成功结果的采集回执，失败的来源可写进 gaps")
		}
	}
	if len(draft.Tables) > 0 {
		return refs, nil
	}
	var rows []observationRow
	if err := tx.Where("receipt_id IN ? AND id IN (?)", draft.AcquisitionReceiptIDs, tx.Model(&taskObservation{}).Select("observation_id").Where("request_id = ?", requestID)).Order("receipt_id, record_position").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return refs, nil
	}
	table := DeliveryTable{Title: defaultDeliveryTableTitle}
	columns := make(map[string]bool)
	for _, row := range rows {
		fact, err := decodeObservation(row)
		if err != nil {
			return nil, err
		}
		table.Rows = append(table.Rows, fact.Fields)
		table.RowObservationIDs = append(table.RowObservationIDs, []string{fact.ID})
		for field := range fact.Fields {
			columns[field] = true
		}
	}
	for field := range columns {
		table.Columns = append(table.Columns, field)
	}
	sort.Strings(table.Columns)
	draft.Tables = []DeliveryTable{table}
	return refs, nil
}
