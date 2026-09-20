package repository

import (
	"context"
	"encoding/json"
	"fmt"
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

// DB 暴露底层连接，供服务层做只读的证据冻结实时重放。
func (r *BalanceRepository) DB() *gorm.DB { return r.db }

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

func (r *BalanceRepository) CreateCalculated(ctx context.Context, run *model.BalanceRun, actor Actor) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(run).Error; err != nil {
			return fmt.Errorf("create calculated balance run: %w", err)
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
		// 摘要检查与状态迁移必须在同一事务内完成：提交/接受/驳回前重放证据，
		// 期间快照或已确认转移变化即拒绝迁移，旧结果保持只读并保留审计。
		if requiresFreshEvidence(target) {
			stale, changes, checkErr := checkRunEvidenceFresh(tx, before)
			if checkErr != nil {
				return checkErr
			}
			if stale {
				if persistErr := persistEvidenceStaleOnGuard(tx, before, changes, actor); persistErr != nil {
					return persistErr
				}
				return api.WithDetails(api.NewError(409, "EVIDENCE_STALE", "平衡证据已过期，必须重新计算生成独立结果后再提交或复核"), map[string]any{
					"balance_run_id": before.ID, "changes": changes,
				})
			}
			if before.EvidenceStale {
				if clearErr := clearEvidenceStaleOnGuard(tx, before, actor); clearErr != nil {
					return clearErr
				}
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

// requiresFreshEvidence 判定一次目标迁移是否必须以证据未过期为前提。
// 作废是管理员处置动作，允许对过期运行执行。
func requiresFreshEvidence(target constants.BalanceStatus) bool {
	return target == constants.BalancePendingReview ||
		target == constants.BalanceAccepted ||
		target == constants.BalanceRejected
}

// checkRunEvidenceFresh 在状态迁移事务内实时重放冻结证据。
func checkRunEvidenceFresh(tx *gorm.DB, run model.BalanceRun) (bool, []model.EvidenceChange, error) {
	manifest, ok, err := model.ParseFrozenManifest(run.FrozenEvidenceJSON)
	if err != nil {
		return false, nil, err
	}
	if !ok {
		return true, []model.EvidenceChange{{Kind: model.EvidenceChangeFreezeMissing, Detail: "该运行缺少证据冻结清单，必须重新计算后才能提交或复核"}}, nil
	}
	opening, err := selectOpeningSnapshot(tx, run.TankID, run.PeriodStart)
	if err != nil {
		return false, nil, err
	}
	closing, err := selectClosingSnapshot(tx, run.TankID, run.PeriodStart, run.PeriodEnd, openingIDOrZero(opening))
	if err != nil {
		return false, nil, err
	}
	transfers, err := selectPeriodTransfers(tx, run.TankID, run.PeriodStart, run.PeriodEnd)
	if err != nil {
		return false, nil, err
	}
	changes, err := model.DiffFrozenEvidence(manifest, opening, closing, transfers, time.Now().UTC())
	if err != nil {
		return false, nil, err
	}
	return len(changes) > 0, changes, nil
}

// persistEvidenceStaleOnGuard 把迁移事务中实时发现的变化来源固化到旧结果，
// 旧结果不删除，过期标记与差异来源随审计链长期保留。
func persistEvidenceStaleOnGuard(tx *gorm.DB, run model.BalanceRun, changes []model.EvidenceChange, actor Actor) error {
	reasonsJSON, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("marshal stale evidence reasons: %w", err)
	}
	if !run.EvidenceStale {
		audit := NewAudit(actor, "balance_run.evidence_stale", "balance_run", run.ID,
			map[string]any{"evidence_stale": false},
			map[string]any{"evidence_stale": true, "changes": changes})
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit stale evidence mark: %w", err)
		}
	}
	result := tx.Model(&model.BalanceRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"evidence_stale": true, "stale_reasons_json": string(reasonsJSON)})
	if result.Error != nil {
		return fmt.Errorf("persist stale evidence mark: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新")
	}
	return nil
}

// clearEvidenceStaleOnGuard 处理持久化标记与实时校验不一致的竞态恢复，
// 摘要检查通过时清除过期标记并留下审计，保证列表状态最终一致。
func clearEvidenceStaleOnGuard(tx *gorm.DB, run model.BalanceRun, actor Actor) error {
	result := tx.Model(&model.BalanceRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"evidence_stale": false, "stale_reasons_json": "[]"})
	if result.Error != nil {
		return fmt.Errorf("clear stale evidence mark: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新")
	}
	audit := NewAudit(actor, "balance_run.evidence_restored", "balance_run", run.ID,
		map[string]any{"evidence_stale": true}, map[string]any{"evidence_stale": false})
	if err := tx.Create(&audit).Error; err != nil {
		return fmt.Errorf("audit restored evidence mark: %w", err)
	}
	return nil
}

func selectOpeningSnapshot(db *gorm.DB, tankID uint, periodStart time.Time) (*model.MeasurementSnapshot, error) {
	var snapshot model.MeasurementSnapshot
	err := db.Where("tank_id = ? AND measured_at <= ? AND quality_flag <> ?", tankID, periodStart.UTC(), constants.QualityInvalid).
		Order("measured_at DESC, id DESC").First(&snapshot).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load current opening snapshot: %w", err)
	}
	return &snapshot, nil
}

func selectClosingSnapshot(db *gorm.DB, tankID uint, periodStart, periodEnd time.Time, openingID uint) (*model.MeasurementSnapshot, error) {
	var snapshot model.MeasurementSnapshot
	err := db.Where("tank_id = ? AND measured_at >= ? AND measured_at <= ? AND quality_flag <> ?", tankID, periodStart.UTC(), periodEnd.UTC(), constants.QualityInvalid).
		Order("measured_at DESC, id DESC").First(&snapshot).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load current closing snapshot: %w", err)
	}
	if snapshot.ID == openingID {
		return nil, nil
	}
	return &snapshot, nil
}

func selectPeriodTransfers(db *gorm.DB, tankID uint, periodStart, periodEnd time.Time) ([]model.TransferOperation, error) {
	items := make([]model.TransferOperation, 0)
	if err := db.Where("tank_id = ? AND operation_status = ? AND start_at >= ? AND end_at <= ?", tankID, "confirmed", periodStart.UTC(), periodEnd.UTC()).
		Order("start_at ASC, id ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("load current confirmed transfers: %w", err)
	}
	return items, nil
}

func loadRunEvidence(db *gorm.DB, run model.BalanceRun) (model.FrozenEvidenceManifest, *model.MeasurementSnapshot, *model.MeasurementSnapshot, []model.TransferOperation, error) {
	manifest, ok, err := model.ParseFrozenManifest(run.FrozenEvidenceJSON)
	if err != nil || !ok {
		return manifest, nil, nil, nil, err
	}
	opening, err := selectOpeningSnapshot(db, run.TankID, run.PeriodStart)
	if err != nil {
		return model.FrozenEvidenceManifest{}, nil, nil, nil, err
	}
	closing, err := selectClosingSnapshot(db, run.TankID, run.PeriodStart, run.PeriodEnd, openingIDOrZero(opening))
	if err != nil {
		return model.FrozenEvidenceManifest{}, nil, nil, nil, err
	}
	transfers, err := selectPeriodTransfers(db, run.TankID, run.PeriodStart, run.PeriodEnd)
	if err != nil {
		return model.FrozenEvidenceManifest{}, nil, nil, nil, err
	}
	return manifest, opening, closing, transfers, nil
}

func openingIDOrZero(opening *model.MeasurementSnapshot) uint {
	if opening == nil {
		return 0
	}
	return opening.ID
}

// EvidenceState 是读取时得到的证据冻结视图。Realtime 为 true 时 Changes 来自
// 当前边界证据重放；为 false 时取自终态运行落库的历史过期原因，仅作展示。
type EvidenceState struct {
	Manifest model.FrozenEvidenceManifest
	Frozen   bool
	Realtime bool
	Changes  []model.EvidenceChange
}

// LoadEvidenceStates 为一批运行装配冻结视图：非终态运行实时重放当前证据，
// 终态运行返回冻结清单与落库的历史变化来源，保证终态结果永不被重新判定。
func LoadEvidenceStates(ctx context.Context, db *gorm.DB, runs []model.BalanceRun) (map[uint]EvidenceState, error) {
	states := make(map[uint]EvidenceState, len(runs))
	guardedRuns := make([]model.BalanceRun, 0, len(runs))
	for _, run := range runs {
		manifest, ok, err := model.ParseFrozenManifest(run.FrozenEvidenceJSON)
		if err != nil {
			return nil, err
		}
		if !constants.EvidenceFreezeGuarded(run.BalanceStatus) {
			states[run.ID] = EvidenceState{Manifest: manifest, Frozen: ok, Realtime: false, Changes: parsePersistedChanges(run.StaleReasonsJSON)}
			continue
		}
		if !ok {
			states[run.ID] = EvidenceState{Frozen: false, Realtime: true, Changes: []model.EvidenceChange{{
				Kind: model.EvidenceChangeFreezeMissing, Detail: "该运行缺少证据冻结清单，必须重新计算后才能提交或复核",
			}}}
			continue
		}
		guardedRuns = append(guardedRuns, run)
	}
	if len(guardedRuns) == 0 {
		return states, nil
	}
	tankIDs := make([]uint, 0)
	seenTank := map[uint]struct{}{}
	minStart, maxEnd := guardedRuns[0].PeriodStart, guardedRuns[0].PeriodEnd
	for _, run := range guardedRuns {
		if _, ok := seenTank[run.TankID]; !ok {
			seenTank[run.TankID] = struct{}{}
			tankIDs = append(tankIDs, run.TankID)
		}
		if run.PeriodStart.Before(minStart) {
			minStart = run.PeriodStart
		}
		if run.PeriodEnd.After(maxEnd) {
			maxEnd = run.PeriodEnd
		}
	}
	snapshotByTank, err := loadReplaySnapshots(ctx, db, tankIDs, maxEnd)
	if err != nil {
		return nil, err
	}
	transferByTank, err := loadReplayTransfers(ctx, db, tankIDs, minStart, maxEnd)
	if err != nil {
		return nil, err
	}
	for _, run := range guardedRuns {
		manifest, ok, err := model.ParseFrozenManifest(run.FrozenEvidenceJSON)
		if err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		opening, closing := pickBoundarySnapshots(snapshotByTank[run.TankID], run.PeriodStart, run.PeriodEnd)
		periodTransfers := pickPeriodTransfers(transferByTank[run.TankID], run.PeriodStart, run.PeriodEnd)
		changes, err := model.DiffFrozenEvidence(manifest, opening, closing, periodTransfers, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		states[run.ID] = EvidenceState{Manifest: manifest, Frozen: true, Realtime: true, Changes: changes}
	}
	return states, nil
}

func loadReplaySnapshots(ctx context.Context, db *gorm.DB, tankIDs []uint, maxEnd time.Time) (map[uint][]model.MeasurementSnapshot, error) {
	var snapshots []model.MeasurementSnapshot
	if err := db.WithContext(ctx).
		Where("tank_id IN ? AND measured_at <= ? AND quality_flag <> ?", tankIDs, maxEnd.UTC(), constants.QualityInvalid).
		Order("measured_at ASC, id ASC").Find(&snapshots).Error; err != nil {
		return nil, fmt.Errorf("load snapshots for evidence replay: %w", err)
	}
	byTank := make(map[uint][]model.MeasurementSnapshot)
	for _, snapshot := range snapshots {
		byTank[snapshot.TankID] = append(byTank[snapshot.TankID], snapshot)
	}
	return byTank, nil
}

func loadReplayTransfers(ctx context.Context, db *gorm.DB, tankIDs []uint, minStart, maxEnd time.Time) (map[uint][]model.TransferOperation, error) {
	var transfers []model.TransferOperation
	if err := db.WithContext(ctx).
		Where("tank_id IN ? AND operation_status = ? AND end_at >= ? AND start_at <= ?", tankIDs, "confirmed", minStart.UTC(), maxEnd.UTC()).
		Order("start_at ASC, id ASC").Find(&transfers).Error; err != nil {
		return nil, fmt.Errorf("load transfers for evidence replay: %w", err)
	}
	byTank := make(map[uint][]model.TransferOperation)
	for _, transfer := range transfers {
		byTank[transfer.TankID] = append(byTank[transfer.TankID], transfer)
	}
	return byTank, nil
}

// parsePersistedChanges 读取随运行落库的历史过期原因，供终态运行展示。
func parsePersistedChanges(raw []byte) []model.EvidenceChange {
	changes := make([]model.EvidenceChange, 0)
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "[]" {
		return changes
	}
	if err := json.Unmarshal(raw, &changes); err != nil {
		return changes
	}
	return changes
}

func pickBoundarySnapshots(history []model.MeasurementSnapshot, periodStart, periodEnd time.Time) (*model.MeasurementSnapshot, *model.MeasurementSnapshot) {
	var opening *model.MeasurementSnapshot
	var closing *model.MeasurementSnapshot
	for index := range history {
		candidate := &history[index]
		if !candidate.MeasuredAt.After(periodStart) {
			if opening == nil || candidate.MeasuredAt.After(opening.MeasuredAt) ||
				(candidate.MeasuredAt.Equal(opening.MeasuredAt) && candidate.ID > opening.ID) {
				opening = candidate
			}
		}
		if !candidate.MeasuredAt.Before(periodStart) && !candidate.MeasuredAt.After(periodEnd) {
			if closing == nil || candidate.MeasuredAt.After(closing.MeasuredAt) ||
				(candidate.MeasuredAt.Equal(closing.MeasuredAt) && candidate.ID > closing.ID) {
				closing = candidate
			}
		}
	}
	if opening != nil && closing != nil && closing.ID == opening.ID {
		closing = nil
	}
	return opening, closing
}

func pickPeriodTransfers(items []model.TransferOperation, periodStart, periodEnd time.Time) []model.TransferOperation {
	result := make([]model.TransferOperation, 0)
	for _, item := range items {
		if !item.StartAt.Before(periodStart) && !item.EndAt.After(periodEnd) {
			result = append(result, item)
		}
	}
	return result
}

// markRunsStaleFromSnapshot 在新增快照事务内重放受影响运行并持久化证据过期标记。
func markRunsStaleFromSnapshot(tx *gorm.DB, tankID uint, changedAt time.Time, actor Actor) error {
	var runs []model.BalanceRun
	if err := tx.Where("tank_id = ? AND balance_status IN ?", tankID, constants.EvidenceGuardedStatuses).Find(&runs).Error; err != nil {
		return fmt.Errorf("select runs affected by snapshot change: %w", err)
	}
	for _, run := range runs {
		manifest, opening, closing, transfers, err := loadRunEvidence(tx, run)
		if err != nil {
			return err
		}
		if err := applyEvidenceStaleMark(tx, run, manifest, opening, closing, transfers, changedAt, actor); err != nil {
			return err
		}
	}
	return nil
}

// markRunsStaleFromTransfer 在确认/取消转移事务内重放受影响运行并持久化证据过期标记。
func markRunsStaleFromTransfer(tx *gorm.DB, tankID uint, changedAt time.Time, actor Actor) error {
	var runs []model.BalanceRun
	if err := tx.Where("tank_id = ? AND balance_status IN ?", tankID, constants.EvidenceGuardedStatuses).Find(&runs).Error; err != nil {
		return fmt.Errorf("select runs affected by transfer change: %w", err)
	}
	for _, run := range runs {
		manifest, opening, closing, transfers, err := loadRunEvidence(tx, run)
		if err != nil {
			return err
		}
		if err := applyEvidenceStaleMark(tx, run, manifest, opening, closing, transfers, changedAt, actor); err != nil {
			return err
		}
	}
	return nil
}

func applyEvidenceStaleMark(tx *gorm.DB, run model.BalanceRun, manifest model.FrozenEvidenceManifest, opening, closing *model.MeasurementSnapshot, transfers []model.TransferOperation, changedAt time.Time, actor Actor) error {
	var changes []model.EvidenceChange
	if manifest.FreezeVersion == "" {
		if run.EvidenceStale {
			return nil
		}
		changes = []model.EvidenceChange{{Kind: model.EvidenceChangeFreezeMissing, Detail: "该运行缺少证据冻结清单，必须重新计算后才能提交或复核", ChangedAt: changedAt.UTC().Format(time.RFC3339Nano)}}
	} else {
		diff, err := model.DiffFrozenEvidence(manifest, opening, closing, transfers, changedAt)
		if err != nil {
			return err
		}
		if len(diff) == 0 {
			return nil
		}
		changes = diff
	}
	reasonsJSON, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("marshal stale evidence reasons: %w", err)
	}
	if !run.EvidenceStale {
		audit := NewAudit(actor, "balance_run.evidence_stale", "balance_run", run.ID,
			map[string]any{"evidence_stale": false},
			map[string]any{"evidence_stale": true, "changes": changes, "detected_at": changedAt.UTC().Format(time.RFC3339Nano)})
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit stale evidence mark: %w", err)
		}
	}
	result := tx.Model(&model.BalanceRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"evidence_stale": true, "stale_reasons_json": string(reasonsJSON)})
	if result.Error != nil {
		return fmt.Errorf("persist stale evidence mark: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新")
	}
	return nil
}
