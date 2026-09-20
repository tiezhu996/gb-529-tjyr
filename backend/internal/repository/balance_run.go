package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

type BalanceFilter struct {
	TankID   uint
	Status   string
	Page     int
	PageSize int
}

type BalanceRepository struct {
	db *gorm.DB
}

func NewBalanceRepository(db *gorm.DB) *BalanceRepository {
	return &BalanceRepository{db: db}
}

func (r *BalanceRepository) List(ctx context.Context, filter BalanceFilter) ([]model.BalanceRun, int64, error) {
	filter.Page, filter.PageSize = normalizePage(filter.Page, filter.PageSize)
	query := r.db.WithContext(ctx).Model(&model.BalanceRun{})
	if filter.TankID > 0 {
		query = query.Where("tank_id = ?", filter.TankID)
	}
	if filter.Status != "" {
		query = query.Where("balance_status = ?", filter.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count balance runs: %w", err)
	}
	var runs []model.BalanceRun
	if err := query.Preload("Tank").Order("created_at DESC, id DESC").
		Offset((filter.Page - 1) * filter.PageSize).Limit(filter.PageSize).Find(&runs).Error; err != nil {
		return nil, 0, fmt.Errorf("list balance runs: %w", err)
	}
	return runs, total, nil
}

func (r *BalanceRepository) Get(ctx context.Context, id uint) (model.BalanceRun, error) {
	var run model.BalanceRun
	if err := r.db.WithContext(ctx).Preload("Tank").First(&run, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return model.BalanceRun{}, api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
		}
		return model.BalanceRun{}, fmt.Errorf("get balance run: %w", err)
	}
	return run, nil
}

func (r *BalanceRepository) CreateCalculated(ctx context.Context, run *model.BalanceRun, freeze *model.BalanceEvidenceFreeze, actor Actor) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(run).Error; err != nil {
			return fmt.Errorf("create calculated balance run: %w", err)
		}
		if freeze != nil {
			freeze.BalanceRunID = run.ID
			if err := tx.Create(freeze).Error; err != nil {
				return fmt.Errorf("create balance evidence freeze: %w", err)
			}
		}
		queuedAudit := NewAudit(actor, "balance_run.queued", "balance_run", run.ID, nil, map[string]any{
			"tank_id": run.TankID, "period_start": run.PeriodStart, "period_end": run.PeriodEnd,
		})
		if err := tx.Create(&queuedAudit).Error; err != nil {
			return fmt.Errorf("audit queued balance run: %w", err)
		}
		calculatedAudit := NewAudit(actor, "balance_run.calculated", "balance_run", run.ID, map[string]any{"status": constants.BalanceQueued}, map[string]any{
			"status": run.BalanceStatus, "estimated_bog_kg": run.EstimatedBOGKG,
			"uncertainty_kg": run.UncertaintyKG, "deviation_level": run.DeviationLevel,
			"coefficient_version": run.CoefficientVersion,
		})
		if err := tx.Create(&calculatedAudit).Error; err != nil {
			return fmt.Errorf("audit balance calculation: %w", err)
		}
		if freeze != nil {
			freezeAudit := NewAudit(actor, "balance_evidence.frozen", "balance_evidence_freeze", freeze.ID, nil, map[string]any{
				"balance_run_id": run.ID, "tank_id": freeze.TankID,
				"opening_digest": freeze.OpeningDigest, "closing_digest": freeze.ClosingDigest,
				"transfer_digest": freeze.TransferDigest, "transfer_count": freeze.TransferCount,
				"snapshot_count": freeze.SnapshotCount, "frozen_at": freeze.FrozenAt,
			})
			if err := tx.Create(&freezeAudit).Error; err != nil {
				return fmt.Errorf("audit frozen evidence summary: %w", err)
			}
		}
		return nil
	})
}

func (r *BalanceRepository) Transition(ctx context.Context, id, version uint, target constants.BalanceStatus, note string, reviewerID *uint, actor Actor) (model.BalanceRun, error) {
	var updated model.BalanceRun
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var before model.BalanceRun
		if err := tx.First(&before, id).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
			}
			return fmt.Errorf("load balance run for transition: %w", err)
		}
		if before.Version != version {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行版本已变化，请刷新后重试")
		}
		if !constants.CanTransitionBalance(before.BalanceStatus, target) {
			return api.WithDetails(api.NewError(409, "INVALID_BALANCE_TRANSITION", "当前平衡状态不允许目标迁移"), map[string]any{
				"current": before.BalanceStatus, "target": target,
			})
		}
		// 摘要检查与复核状态迁移必须在同一事务内完成：
		// 过期证据在事务内被拦下，状态机和冻结核验不会出现跨请求不一致。
		var frozen model.BalanceEvidenceFreeze
		freezeFound := true
		if err := tx.Where("balance_run_id = ?", id).First(&frozen).Error; err != nil {
			if err != gorm.ErrRecordNotFound {
				return fmt.Errorf("load evidence freeze for transition: %w", err)
			}
			freezeFound = false
		}
		if freezeFound {
			fc, err := r.loadFreezeContext(tx, frozen.TankID, frozen.PeriodStart, frozen.PeriodEnd)
			if err != nil {
				return err
			}
			evaluation, err := EvaluateFreeze(frozen, fc)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			if frozen.FreezeStatus != evaluation.Status || frozen.StaleReason != evaluation.StaleReason {
				if err := tx.Model(&model.BalanceEvidenceFreeze{}).Where("id = ?", frozen.ID).
					Updates(map[string]any{"freeze_status": evaluation.Status, "stale_reason": evaluation.StaleReason, "checked_at": now, "updated_at": now}).Error; err != nil {
					return fmt.Errorf("sync freeze status during transition: %w", err)
				}
				audit := NewAudit(actor, "balance_evidence."+string(evaluation.Status), "balance_evidence_freeze", frozen.ID,
					map[string]any{"freeze_status": frozen.FreezeStatus},
					map[string]any{"freeze_status": evaluation.Status, "stale_reason": evaluation.StaleReason, "balance_run_id": id, "changes": evaluation.Changes})
				if err := tx.Create(&audit).Error; err != nil {
					return fmt.Errorf("audit freeze status during transition: %w", err)
				}
			} else {
				if err := tx.Model(&model.BalanceEvidenceFreeze{}).Where("id = ?", frozen.ID).
					Updates(map[string]any{"checked_at": now, "updated_at": now}).Error; err != nil {
					return fmt.Errorf("touch freeze check during transition: %w", err)
				}
			}
			// 提交复核与接受/驳回只允许针对当前证据；作废仍可对过期执行，旧结果和审计保留。
			if evaluation.Status == model.EvidenceFreezeStale && target != constants.BalanceInvalidated {
				return api.WithDetails(api.NewError(409, "BALANCE_EVIDENCE_STALE",
					"平衡运行证据已过期，必须重新计算生成独立结果后再提交或复核"), map[string]any{
					"balance_run_id": id,
					"freeze_status":  string(evaluation.Status),
					"stale_reason":   evaluation.StaleReason,
					"changes":        evaluation.Changes,
					"target":         target,
				})
			}
		}
		updates := map[string]any{
			"balance_status": target,
			"version":        gorm.Expr("version + 1"),
			"review_note":    note,
		}
		if reviewerID != nil {
			now := time.Now().UTC()
			updates["reviewed_by"] = *reviewerID
			updates["reviewed_at"] = now
		}
		result := tx.Model(&model.BalanceRun{}).
			Where("id = ? AND version = ? AND balance_status = ?", id, version, before.BalanceStatus).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("transition balance run: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新")
		}
		if err := tx.First(&updated, id).Error; err != nil {
			return fmt.Errorf("reload balance run: %w", err)
		}
		audit := NewAudit(actor, "balance_run."+string(target), "balance_run", id, before, updated)
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit balance transition: %w", err)
		}
		return nil
	})
	return updated, err
}

// freezeSchemaVersion 标记固化摘要结构版本，摘要结构演进时可区分旧证据。
const freezeSchemaVersion = 1

// FreezeContext 是一次证据核验时刻从数据库读出的当前证据上下文。
type FreezeContext struct {
	Snapshots []model.MeasurementSnapshot
	Transfers []model.TransferOperation
}

// FreezeEvaluation 是固化摘要与当前证据比对后的结论。
type FreezeEvaluation struct {
	BalanceRunID uint
	FreezeID     uint
	Status       model.EvidenceFreezeStatus
	Changes      []model.FreezeChangeSource
	StaleReason  string
	Opening      model.FrozenSnapshotItem
	Closing      model.FrozenSnapshotItem
	Transfers    []model.FrozenTransferItem
	PeriodShots  []model.FrozenSnapshotItem
	Checked      bool
}

// summarizeSnapshot 将不可变快照裁剪为参与平衡证据的摘要字段。
func summarizeSnapshot(snapshot model.MeasurementSnapshot) model.FrozenSnapshotItem {
	return model.FrozenSnapshotItem{
		ID:                        snapshot.ID,
		MeasuredAt:                snapshot.MeasuredAt.UTC().Format(time.RFC3339),
		LiquidLevelM:              snapshot.LiquidLevelM,
		LiquidTempC:               snapshot.LiquidTempC,
		VaporPressureKPA:          snapshot.VaporPressureKPA,
		DensityKGM3:               snapshot.DensityKGM3,
		CalculatedLiquidMassKG:    snapshot.CalculatedLiquidMassKG,
		MeasurementUncertaintyPct: snapshot.MeasurementUncertaintyPct,
		QualityFlag:               string(snapshot.QualityFlag),
	}
}

func summarizeTransfer(transfer model.TransferOperation) model.FrozenTransferItem {
	return model.FrozenTransferItem{
		ID:                        transfer.ID,
		OperationType:             transfer.OperationType,
		StartAt:                   transfer.StartAt.UTC().Format(time.RFC3339),
		EndAt:                     transfer.EndAt.UTC().Format(time.RFC3339),
		MeasuredMassKG:            transfer.MeasuredMassKG,
		MeasurementUncertaintyPct: transfer.MeasurementUncertaintyPct,
		OperationStatus:           transfer.OperationStatus,
		CounterpartyRef:           transfer.CounterpartyRef,
	}
}

// digestJSON 以结构声明顺序做确定性 JSON 序列化并取 SHA-256，避免映射迭代顺序不稳定。
func digestJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize freeze digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BuildEvidenceFreeze 在计算事务内根据期初、期末快照、期间有效快照集合和已确认转移
// 构造独立的证据冻结记录（含三个摘要摘要值）。
func BuildEvidenceFreeze(tankID uint, start, end time.Time, opening, closing model.MeasurementSnapshot, period []model.MeasurementSnapshot, transfers []model.TransferOperation, frozenAt time.Time) (model.BalanceEvidenceFreeze, error) {
	sort.Slice(period, func(i, j int) bool {
		if period[i].MeasuredAt.Equal(period[j].MeasuredAt) {
			return period[i].ID < period[j].ID
		}
		return period[i].MeasuredAt.Before(period[j].MeasuredAt)
	})
	sort.Slice(transfers, func(i, j int) bool {
		if transfers[i].StartAt.Equal(transfers[j].StartAt) {
			return transfers[i].ID < transfers[j].ID
		}
		return transfers[i].StartAt.Before(transfers[j].StartAt)
	})
	periodItems := make([]model.FrozenSnapshotItem, 0, len(period))
	for _, snapshot := range period {
		periodItems = append(periodItems, summarizeSnapshot(snapshot))
	}
	transferItems := make([]model.FrozenTransferItem, 0, len(transfers))
	for _, transfer := range transfers {
		transferItems = append(transferItems, summarizeTransfer(transfer))
	}
	openingItem := summarizeSnapshot(opening)
	closingItem := summarizeSnapshot(closing)
	openingDigest, err := digestJSON(openingItem)
	if err != nil {
		return model.BalanceEvidenceFreeze{}, err
	}
	closingDigest, err := digestJSON(closingItem)
	if err != nil {
		return model.BalanceEvidenceFreeze{}, err
	}
	transferDigest, err := digestJSON(transferItems)
	if err != nil {
		return model.BalanceEvidenceFreeze{}, err
	}
	summary := model.EvidenceFreezeSummary{
		SchemaVersion:      freezeSchemaVersion,
		FrozenAt:           frozenAt.UTC().Format(time.RFC3339),
		TankID:             tankID,
		PeriodStart:        start.UTC().Format(time.RFC3339),
		PeriodEnd:          end.UTC().Format(time.RFC3339),
		OpeningSnapshot:    openingItem,
		ClosingSnapshot:    closingItem,
		PeriodSnapshots:    periodItems,
		ConfirmedTransfers: transferItems,
	}
	return model.BalanceEvidenceFreeze{
		TankID:         tankID,
		PeriodStart:    start.UTC(),
		PeriodEnd:      end.UTC(),
		FreezeStatus:   model.EvidenceFreezeFrozen,
		OpeningDigest:  openingDigest,
		ClosingDigest:  closingDigest,
		TransferDigest: transferDigest,
		TransferCount:  len(transferItems),
		SnapshotCount:  len(periodItems),
		SummaryJSON:    model.MustMarshalFreezeSummary(summary),
		FrozenAt:       frozenAt.UTC(),
		CheckedAt:      frozenAt.UTC(),
	}, nil
}

func pickBoundarySnapshots(snapshots []model.MeasurementSnapshot, start, end time.Time) (opening, closing model.MeasurementSnapshot, period []model.MeasurementSnapshot, ok bool) {
	for _, snapshot := range snapshots {
		measured := snapshot.MeasuredAt.UTC()
		if !measured.After(start.UTC()) {
			if opening.ID == 0 || measured.After(opening.MeasuredAt.UTC()) || (measured.Equal(opening.MeasuredAt.UTC()) && snapshot.ID > opening.ID) {
				opening = snapshot
			}
		}
		if !measured.Before(start.UTC()) && !measured.After(end.UTC()) {
			if closing.ID == 0 || measured.After(closing.MeasuredAt.UTC()) || (measured.Equal(closing.MeasuredAt.UTC()) && snapshot.ID > closing.ID) {
				closing = snapshot
			}
		}
		if measured.After(start.UTC()) && !measured.After(end.UTC()) {
			period = append(period, snapshot)
		}
	}
	ok = opening.ID != 0 && closing.ID != 0 && closing.ID != opening.ID && closing.MeasuredAt.After(opening.MeasuredAt)
	return
}

func pickConfirmedTransfers(items []model.TransferOperation, start, end time.Time) []model.TransferOperation {
	filtered := make([]model.TransferOperation, 0)
	for _, item := range items {
		if item.OperationStatus != "confirmed" {
			continue
		}
		if !item.StartAt.UTC().Before(start.UTC()) && !item.EndAt.UTC().After(end.UTC()) {
			filtered = append(filtered, item)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].StartAt.Equal(filtered[j].StartAt) {
			return filtered[i].ID < filtered[j].ID
		}
		return filtered[i].StartAt.Before(filtered[j].StartAt)
	})
	return filtered
}

// EvaluateFreeze 用当前证据上下文重新计算边界与转移摘要，并与固化摘要逐项比对。
func EvaluateFreeze(frozen model.BalanceEvidenceFreeze, fc FreezeContext) (FreezeEvaluation, error) {
	var summary model.EvidenceFreezeSummary
	if err := json.Unmarshal(frozen.SummaryJSON, &summary); err != nil {
		return FreezeEvaluation{}, fmt.Errorf("decode frozen evidence summary: %w", err)
	}
	opening, closing, period, ok := pickBoundarySnapshots(fc.Snapshots, frozen.PeriodStart, frozen.PeriodEnd)
	transfers := pickConfirmedTransfers(fc.Transfers, frozen.PeriodStart, frozen.PeriodEnd)
	evaluation := FreezeEvaluation{
		BalanceRunID: frozen.BalanceRunID,
		FreezeID:     frozen.ID,
		Status:       model.EvidenceFreezeFrozen,
		Checked:      true,
	}
	if !ok {
		evaluation.Status = model.EvidenceFreezeStale
		evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
			Kind:       model.FreezeSourceSnapshotAdded,
			EntityType: "measurement_snapshot",
			Message:    "固化的期初或期末边界快照已无法重新定位，必须重新计算平衡",
		})
	} else {
		openingItem := summarizeSnapshot(opening)
		closingItem := summarizeSnapshot(closing)
		evaluation.Opening = openingItem
		evaluation.Closing = closingItem
		openingDigest, err := digestJSON(openingItem)
		if err != nil {
			return FreezeEvaluation{}, err
		}
		closingDigest, err := digestJSON(closingItem)
		if err != nil {
			return FreezeEvaluation{}, err
		}
		if openingDigest != frozen.OpeningDigest {
			evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
				Kind:       model.FreezeSourceOpeningSnapshot,
				EntityType: "measurement_snapshot",
				EntityID:   opening.ID,
				OccurredAt: opening.MeasuredAt.UTC().Format(time.RFC3339),
				Message: fmt.Sprintf("期初边界快照已由 #%d 切换为 #%d（%s）",
					summary.OpeningSnapshot.ID, opening.ID, opening.MeasuredAt.UTC().Format(time.RFC3339)),
			})
		}
		if closingDigest != frozen.ClosingDigest {
			evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
				Kind:       model.FreezeSourceClosingSnapshot,
				EntityType: "measurement_snapshot",
				EntityID:   closing.ID,
				OccurredAt: closing.MeasuredAt.UTC().Format(time.RFC3339),
				Message: fmt.Sprintf("期末边界快照已由 #%d 切换为 #%d（%s）",
					summary.ClosingSnapshot.ID, closing.ID, closing.MeasuredAt.UTC().Format(time.RFC3339)),
			})
		}
		known := make(map[uint]struct{}, len(summary.PeriodSnapshots))
		for _, item := range summary.PeriodSnapshots {
			known[item.ID] = struct{}{}
		}
		for _, snapshot := range period {
			if _, exists := known[snapshot.ID]; exists {
				continue
			}
			evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
				Kind:       model.FreezeSourceSnapshotAdded,
				EntityType: "measurement_snapshot",
				EntityID:   snapshot.ID,
				OccurredAt: snapshot.MeasuredAt.UTC().Format(time.RFC3339),
				Message: fmt.Sprintf("期间新增计量快照 #%d（%s，液位 %.3f m，质量 %s kg）",
					snapshot.ID, snapshot.MeasuredAt.UTC().Format(time.RFC3339), snapshot.LiquidLevelM,
					formatMass(snapshot.CalculatedLiquidMassKG)),
			})
		}
	}
	currentItems := make([]model.FrozenTransferItem, 0, len(transfers))
	currentIDs := make(map[uint]model.TransferOperation, len(transfers))
	for _, transfer := range transfers {
		currentItems = append(currentItems, summarizeTransfer(transfer))
		currentIDs[transfer.ID] = transfer
	}
	evaluation.Transfers = currentItems
	periodItems := make([]model.FrozenSnapshotItem, 0, len(period))
	for _, snapshot := range period {
		periodItems = append(periodItems, summarizeSnapshot(snapshot))
	}
	evaluation.PeriodShots = periodItems
	currentTransferDigest, err := digestJSON(currentItems)
	if err != nil {
		return FreezeEvaluation{}, err
	}
	if currentTransferDigest != frozen.TransferDigest {
		frozenIDs := make(map[uint]struct{}, len(summary.ConfirmedTransfers))
		for _, item := range summary.ConfirmedTransfers {
			frozenIDs[item.ID] = struct{}{}
		}
		for _, transfer := range transfers {
			if _, exists := frozenIDs[transfer.ID]; exists {
				continue
			}
			evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
				Kind:       model.FreezeSourceTransferConfirmed,
				EntityType: "transfer_operation",
				EntityID:   transfer.ID,
				OccurredAt: transfer.UpdatedAt.UTC().Format(time.RFC3339),
				Message: fmt.Sprintf("期间转移 #%d 新获确认（%s，%s kg）",
					transfer.ID, transferOperationLabel(transfer.OperationType), formatMass(transfer.MeasuredMassKG)),
			})
		}
		for _, item := range summary.ConfirmedTransfers {
			if _, exists := currentIDs[item.ID]; exists {
				continue
			}
			evaluation.Changes = append(evaluation.Changes, model.FreezeChangeSource{
				Kind:       model.FreezeSourceTransferUnlocked,
				EntityType: "transfer_operation",
				EntityID:   item.ID,
				Message:    fmt.Sprintf("固化时已确认的转移 #%d 已取消，不再计入期间汇总", item.ID),
			})
		}
	}
	if len(evaluation.Changes) > 0 {
		evaluation.Status = model.EvidenceFreezeStale
		evaluation.StaleReason = joinStaleReasons(evaluation.Changes)
	}
	return evaluation, nil
}

func transferOperationLabel(kind string) string {
	if kind == "inflow" {
		return "流入"
	}
	return "流出"
}

func formatMass(value float64) string {
	return fmt.Sprintf("%.0f", value)
}

func joinStaleReasons(changes []model.FreezeChangeSource) string {
	messages := make([]string, 0, 3)
	for index, change := range changes {
		if index >= 3 {
			break
		}
		messages = append(messages, change.Message)
	}
	joined := strings.Join(messages, "；")
	if len(changes) > 3 {
		joined += fmt.Sprintf("；另有 %d 项变化", len(changes)-3)
	}
	if len(joined) > 480 {
		return joined[:480]
	}
	return joined
}

// loadFreezeContext 在指定事务/连接内读取一次证据核验所需的全部快照和转移。
func (r *BalanceRepository) loadFreezeContext(query *gorm.DB, tankID uint, start, end time.Time) (FreezeContext, error) {
	var snapshots []model.MeasurementSnapshot
	if err := query.
		Where("tank_id = ? AND measured_at <= ? AND quality_flag <> ?", tankID, end.UTC(), constants.QualityInvalid).
		Order("measured_at ASC, id ASC").Find(&snapshots).Error; err != nil {
		return FreezeContext{}, fmt.Errorf("load freeze snapshot context: %w", err)
	}
	var transfers []model.TransferOperation
	if err := query.
		Where("tank_id = ? AND operation_status = ? AND start_at >= ? AND end_at <= ?", tankID, "confirmed", start.UTC(), end.UTC()).
		Order("start_at ASC, id ASC").Find(&transfers).Error; err != nil {
		return FreezeContext{}, fmt.Errorf("load freeze transfer context: %w", err)
	}
	return FreezeContext{Snapshots: snapshots, Transfers: transfers}, nil
}

// ListFreezesByRunIDs 批量读取运行的固化摘要，避免列表场景 N+1 查询。
func (r *BalanceRepository) ListFreezesByRunIDs(ctx context.Context, runIDs []uint) (map[uint]model.BalanceEvidenceFreeze, error) {
	result := make(map[uint]model.BalanceEvidenceFreeze, len(runIDs))
	if len(runIDs) == 0 {
		return result, nil
	}
	var freezes []model.BalanceEvidenceFreeze
	if err := r.db.WithContext(ctx).Where("balance_run_id IN ?", runIDs).Find(&freezes).Error; err != nil {
		return nil, fmt.Errorf("list balance evidence freezes: %w", err)
	}
	for _, freeze := range freezes {
		result[freeze.BalanceRunID] = freeze
	}
	return result, nil
}

// LoadFreezeContexts 批量读取多个固化记录的当前证据上下文，按冻结记录 ID 分组。
func (r *BalanceRepository) LoadFreezeContexts(ctx context.Context, freezes []model.BalanceEvidenceFreeze) (map[uint]FreezeContext, error) {
	contexts := make(map[uint]FreezeContext, len(freezes))
	if len(freezes) == 0 {
		return contexts, nil
	}
	tankIDs := make([]uint, 0, len(freezes))
	seenTanks := make(map[uint]struct{})
	var minStart time.Time
	var maxEnd time.Time
	for _, freeze := range freezes {
		if _, ok := seenTanks[freeze.TankID]; !ok {
			seenTanks[freeze.TankID] = struct{}{}
			tankIDs = append(tankIDs, freeze.TankID)
		}
		if minStart.IsZero() || freeze.PeriodStart.Before(minStart) {
			minStart = freeze.PeriodStart
		}
		if maxEnd.IsZero() || freeze.PeriodEnd.After(maxEnd) {
			maxEnd = freeze.PeriodEnd
		}
	}
	var snapshots []model.MeasurementSnapshot
	if err := r.db.WithContext(ctx).
		Where("tank_id IN ? AND measured_at <= ? AND quality_flag <> ?", tankIDs, maxEnd.UTC(), constants.QualityInvalid).
		Order("measured_at ASC, id ASC").Find(&snapshots).Error; err != nil {
		return nil, fmt.Errorf("batch load freeze snapshots: %w", err)
	}
	var transfers []model.TransferOperation
	if err := r.db.WithContext(ctx).
		Where("tank_id IN ? AND operation_status = ? AND start_at >= ? AND end_at <= ?", tankIDs, "confirmed", minStart.UTC(), maxEnd.UTC()).
		Order("start_at ASC, id ASC").Find(&transfers).Error; err != nil {
		return nil, fmt.Errorf("batch load freeze transfers: %w", err)
	}
	for _, freeze := range freezes {
		fc := FreezeContext{Snapshots: make([]model.MeasurementSnapshot, 0), Transfers: make([]model.TransferOperation, 0)}
		for _, snapshot := range snapshots {
			if snapshot.TankID == freeze.TankID {
				fc.Snapshots = append(fc.Snapshots, snapshot)
			}
		}
		for _, transfer := range transfers {
			if transfer.TankID == freeze.TankID {
				fc.Transfers = append(fc.Transfers, transfer)
			}
		}
		contexts[freeze.BalanceRunID] = fc
	}
	return contexts, nil
}

// PersistFreezeEvaluations 将懒核验发现的状态漂移一次性落库，并为每次状态翻转写审计；
// 核验结论与状态迁移无关，仅同步 freeze_status / stale_reason / checked_at。
func (r *BalanceRepository) PersistFreezeEvaluations(ctx context.Context, freezes map[uint]model.BalanceEvidenceFreeze, evaluations map[uint]FreezeEvaluation, actor Actor) error {
	if len(evaluations) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for runID, evaluation := range evaluations {
			frozen, ok := freezes[runID]
			if !ok {
				continue
			}
			if frozen.FreezeStatus == evaluation.Status && frozen.StaleReason == evaluation.StaleReason {
				continue
			}
			result := tx.Model(&model.BalanceEvidenceFreeze{}).
				Where("id = ?", frozen.ID).
				Updates(map[string]any{
					"freeze_status": evaluation.Status,
					"stale_reason":  evaluation.StaleReason,
					"checked_at":    now,
					"updated_at":    now,
				})
			if result.Error != nil {
				return fmt.Errorf("persist freeze evaluation for run %d: %w", runID, result.Error)
			}
			action := "balance_evidence.verified"
			if evaluation.Status == model.EvidenceFreezeStale {
				action = "balance_evidence.stale"
			}
			audit := NewAudit(actor, action, "balance_evidence_freeze", frozen.ID,
				map[string]any{"freeze_status": frozen.FreezeStatus, "stale_reason": frozen.StaleReason},
				map[string]any{"freeze_status": evaluation.Status, "stale_reason": evaluation.StaleReason, "balance_run_id": runID, "changes": evaluation.Changes})
			if err := tx.Create(&audit).Error; err != nil {
				return fmt.Errorf("audit freeze evaluation for run %d: %w", runID, err)
			}
		}
		return nil
	})
}

// markEvidenceStaleForSnapshot 在新增计量快照的同一事务内，把可能受影响的固化证据标过期。
// 懒核验仍是状态真相来源，这里采用保守窗口，误标会在下次读取时自愈。
func markEvidenceStaleForSnapshot(tx *gorm.DB, snapshot model.MeasurementSnapshot) error {
	now := time.Now().UTC()
	reason := fmt.Sprintf("新增计量快照 #%d，证据需重新核验", snapshot.ID)
	return tx.Model(&model.BalanceEvidenceFreeze{}).
		Where("tank_id = ? AND freeze_status = ? AND period_end >= ?", snapshot.TankID, model.EvidenceFreezeFrozen, snapshot.MeasuredAt.UTC()).
		Updates(map[string]any{"freeze_status": model.EvidenceFreezeStale, "stale_reason": reason, "checked_at": now, "updated_at": now}).Error
}

// markEvidenceStaleForTransfer 在转移确认/取消的同一事务内标记窗口重叠的固化证据。
func markEvidenceStaleForTransfer(tx *gorm.DB, transfer model.TransferOperation) error {
	now := time.Now().UTC()
	reason := fmt.Sprintf("期间转移 #%d 确认状态变化，证据需重新核验", transfer.ID)
	return tx.Model(&model.BalanceEvidenceFreeze{}).
		Where("tank_id = ? AND freeze_status = ? AND period_start <= ? AND period_end >= ?",
			transfer.TankID, model.EvidenceFreezeFrozen, transfer.StartAt.UTC(), transfer.EndAt.UTC()).
		Updates(map[string]any{"freeze_status": model.EvidenceFreezeStale, "stale_reason": reason, "checked_at": now, "updated_at": now}).Error
}
