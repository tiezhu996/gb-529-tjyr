package service

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/balance"
	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/internal/repository"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

func newFreezeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:balance-evidence-freeze-" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.StorageTank{}, &model.MeasurementSnapshot{},
		&model.TransferOperation{}, &model.BalanceRun{}, &model.AuditEvent{},
	); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	return db
}

func newFreezeServices(db *gorm.DB) (*BalanceService, *MeasurementService, *TransferService) {
	tankRepo := repository.NewTankRepository(db)
	measurementRepo := repository.NewMeasurementRepository(db)
	transferRepo := repository.NewTransferRepository(db)
	balanceRepo := repository.NewBalanceRepository(db)
	return NewBalanceService(balanceRepo, tankRepo, measurementRepo, transferRepo),
		NewMeasurementService(measurementRepo, tankRepo),
		NewTransferService(transferRepo, tankRepo)
}

func seedFreezeTank(t *testing.T, db *gorm.DB) model.StorageTank {
	t.Helper()
	curve, err := balance.NewCapacityCurve([]float64{0, 15000})
	if err != nil {
		t.Fatalf("capacity curve: %v", err)
	}
	raw, err := curve.Marshal()
	if err != nil {
		t.Fatalf("marshal curve: %v", err)
	}
	tank := model.StorageTank{
		TankCode: "TK-FREEZE", Name: "证据冻结测试罐", NominalCapacityM3: 180000,
		MinLevelM: 0, MaxLevelM: 12, ReferenceDensityKGM3: 452, ReferenceTemperatureC: -160,
		ThermalExpansionPerC: 0.0035, CapacityCurveJSON: raw,
		CoefficientVersion: "CV-FREEZE-1", TankStatus: "active", Version: 1,
	}
	if err := db.Create(&tank).Error; err != nil {
		t.Fatalf("create tank: %v", err)
	}
	return tank
}

func createFreezeSnapshot(t *testing.T, service *MeasurementService, actor repository.Actor, tankID uint, measuredAt time.Time, level, uncertainty float64, quality constants.QualityFlag) model.MeasurementSnapshot {
	t.Helper()
	snapshot, err := service.Create(context.Background(), dto.CreateMeasurementRequest{
		TankID: tankID, MeasuredAt: &measuredAt, LiquidLevelM: level, LiquidTempC: -160.1,
		VaporPressureKPA: 111, DensityKGM3: 451.5, MeasurementUncertaintyPct: uncertainty,
		QualityFlag: string(quality), SourceNote: "证据冻结流程测试录入",
	}, actor)
	if err != nil {
		t.Fatalf("create snapshot at %s: %v", measuredAt.Format(time.RFC3339), err)
	}
	return snapshot
}

func createFreezeTransfer(t *testing.T, service *TransferService, actor repository.Actor, tankID uint, opType string, start, end time.Time, mass float64, status string) model.TransferOperation {
	t.Helper()
	item, err := service.Create(context.Background(), dto.CreateTransferRequest{
		TankID: tankID, OperationType: opType, StartAt: &start, EndAt: &end,
		MeasuredMassKG: mass, MeasurementUncertaintyPct: 0.25,
		CounterpartyRef: "FREEZE-METER-01", OperationStatus: status,
	}, actor)
	if err != nil {
		t.Fatalf("create %s transfer: %v", opType, err)
	}
	return item
}

func TestBalanceEvidenceFreezeFlowEndToEnd(t *testing.T) {
	db := newFreezeTestDB(t)
	balanceService, measurementService, transferService := newFreezeServices(db)
	tank := seedFreezeTank(t, db)
	analyst := repository.Actor{UserID: 1, Email: "analyst@lng.local", Role: constants.RoleProcessAnalyst, RequestID: "req-freeze-analyst"}
	reviewer := repository.Actor{UserID: 2, Email: "reviewer@lng.local", Role: constants.RoleReviewer, RequestID: "req-freeze-reviewer"}
	admin := repository.Actor{UserID: 3, Email: "admin@lng.local", Role: constants.RoleAdmin, RequestID: "req-freeze-admin"}

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-24 * time.Hour)
	periodEnd := now.Add(-1 * time.Hour)
	opening := createFreezeSnapshot(t, measurementService, analyst, tank.ID, periodStart.Add(-time.Hour), 8.2, 0.3, constants.QualityGood)
	closing := createFreezeSnapshot(t, measurementService, analyst, tank.ID, periodEnd.Add(-2*time.Hour), 8.1, 0.32, constants.QualityGood)
	inflow := createFreezeTransfer(t, transferService, analyst, tank.ID, "inflow",
		periodStart.Add(4*time.Hour), periodStart.Add(5*time.Hour), 250000, "confirmed")
	outflow := createFreezeTransfer(t, transferService, analyst, tank.ID, "outflow",
		periodStart.Add(8*time.Hour), periodStart.Add(9*time.Hour), 90000, "draft")

	// 1. 计算时固化期初、期末快照与期间已确认转移摘要。
	run, err := balanceService.Run(context.Background(), dto.RunBalanceRequest{
		TankID: tank.ID, PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}, analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	if run.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("new run status = %s, want calculating", run.BalanceStatus)
	}
	if run.FrozenManifest == nil || run.FrozenManifest.FreezeVersion != model.EvidenceFreezeV1 {
		t.Fatalf("new run missing frozen manifest: %+v", run.FrozenManifest)
	}
	if run.FrozenManifest.OpeningSnapshot.SnapshotID != opening.ID || run.FrozenManifest.ClosingSnapshot.SnapshotID != closing.ID {
		t.Fatalf("frozen boundary mismatch: %+v", run.FrozenManifest)
	}
	if len(run.FrozenManifest.Transfers) != 1 || run.FrozenManifest.Transfers[0].TransferID != inflow.ID {
		t.Fatalf("frozen transfers should only contain confirmed inflow, got %+v", run.FrozenManifest.Transfers)
	}
	if run.EvidenceStale || len(run.StaleChanges) != 0 {
		t.Fatalf("fresh run must not be stale: %+v", run.StaleChanges)
	}
	frozenNetTransfer := run.NetTransferKG

	// 2. 确认草稿流出：期间已确认集合变化，旧运行同事务被标记过期。
	confirmedOutflow, err := transferService.Transition(context.Background(), outflow.ID, dto.TransitionTransferRequest{
		TargetStatus: "confirmed", Version: outflow.Version, Reason: "证据冻结测试确认流出",
	}, analyst)
	if err != nil {
		t.Fatalf("confirm outflow: %v", err)
	}
	staleView, err := balanceService.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if !staleView.EvidenceStale {
		t.Fatal("run must be marked evidence stale after transfer confirmation")
	}
	if len(staleView.StaleChanges) != 1 || staleView.StaleChanges[0].Kind != model.EvidenceChangeTransferAdded || staleView.StaleChanges[0].EntityID != confirmedOutflow.ID {
		t.Fatalf("expected added outflow change, got %+v", staleView.StaleChanges)
	}

	// 3. 过期运行不得提交复核；摘要检查与状态迁移在同一事务，运行仍停留在 calculating。
	if _, err := balanceService.Submit(context.Background(), run.ID, dto.SubmitBalanceRequest{Version: run.Version}, analyst); err == nil {
		t.Fatal("stale run must not be submitted for review")
	} else if appErr, ok := err.(*api.Error); !ok || appErr.Code != "EVIDENCE_STALE" {
		t.Fatalf("expected EVIDENCE_STALE conflict, got %v", err)
	}
	blockedRun, err := balanceService.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload blocked run: %v", err)
	}
	if blockedRun.BalanceStatus != constants.BalanceCalculating || !blockedRun.EvidenceStale {
		t.Fatalf("blocked submit must not move status: %+v", blockedRun.BalanceStatus)
	}

	// 4. 管理员作废仍然允许，证据链保留。
	invalidated, err := balanceService.Invalidate(context.Background(), run.ID, dto.InvalidateBalanceRequest{
		Version: run.Version, Reason: "证据已过期，转独立结果重算",
	}, admin)
	if err != nil {
		t.Fatalf("invalidate stale run: %v", err)
	}
	if invalidated.BalanceStatus != constants.BalanceInvalidated {
		t.Fatalf("invalidate status = %s", invalidated.BalanceStatus)
	}
	// 终态运行保留冻结清单与历史过期原因，但不再参与实时重放判定。
	if invalidated.FrozenManifest == nil || invalidated.FrozenManifest.FreezeVersion != model.EvidenceFreezeV1 {
		t.Fatal("invalidated run must retain the frozen manifest for audit replay")
	}
	if !invalidated.EvidenceStale || len(invalidated.StaleChanges) != 1 {
		t.Fatalf("invalidated run must keep historical stale reasons, got stale=%v changes=%+v", invalidated.EvidenceStale, invalidated.StaleChanges)
	}

	// 5. 重新计算生成独立结果：新运行包含两条确认转移，旧结果不被覆盖。
	recalculated, err := balanceService.Run(context.Background(), dto.RunBalanceRequest{
		TankID: tank.ID, PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}, analyst)
	if err != nil {
		t.Fatalf("recalculate balance: %v", err)
	}
	if recalculated.ID == run.ID {
		t.Fatal("recalculation must create an independent balance run record")
	}
	if len(recalculated.FrozenManifest.Transfers) != 2 {
		t.Fatalf("new run must freeze both confirmed transfers, got %d", len(recalculated.FrozenManifest.Transfers))
	}
	if recalculated.NetTransferKG == frozenNetTransfer {
		t.Fatal("new run net transfer must reflect the additional confirmed outflow")
	}
	if recalculated.EvidenceStale {
		t.Fatal("newly calculated run must start with fresh evidence")
	}
	oldRun, err := balanceService.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("load old run: %v", err)
	}
	if oldRun.BalanceStatus != constants.BalanceInvalidated || oldRun.NetTransferKG != frozenNetTransfer {
		t.Fatalf("old result and audit trail must remain intact, got status=%s net=%f", oldRun.BalanceStatus, oldRun.NetTransferKG)
	}

	// 6. 新结果提交后，期间再新增快照，待复核运行过期且不得接受/驳回。
	submitted, err := balanceService.Submit(context.Background(), recalculated.ID, dto.SubmitBalanceRequest{Version: recalculated.Version}, analyst)
	if err != nil {
		t.Fatalf("submit fresh run: %v", err)
	}
	if submitted.BalanceStatus != constants.BalancePendingReview {
		t.Fatalf("submitted status = %s", submitted.BalanceStatus)
	}
	intruder := createFreezeSnapshot(t, measurementService, analyst, tank.ID, periodEnd.Add(-30*time.Minute), 7.9, 0.35, constants.QualityGood)
	pendingStale, err := balanceService.Get(context.Background(), recalculated.ID)
	if err != nil {
		t.Fatalf("reload pending run: %v", err)
	}
	if !pendingStale.EvidenceStale {
		t.Fatal("pending review run must become stale after new period snapshot")
	}
	if len(pendingStale.StaleChanges) != 1 || pendingStale.StaleChanges[0].EntityID != intruder.ID {
		t.Fatalf("expected intruder snapshot change, got %+v", pendingStale.StaleChanges)
	}
	if _, err := balanceService.Review(context.Background(), recalculated.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "复核尝试接受过期证据",
	}, reviewer); err == nil {
		t.Fatal("reviewer must not accept stale pending run")
	} else if appErr, ok := err.(*api.Error); !ok || appErr.Code != "EVIDENCE_STALE" {
		t.Fatalf("expected EVIDENCE_STALE on review, got %v", err)
	}

	// 7. 列表视图对每条运行同时暴露冻结状态与变化来源。
	items, total, err := balanceService.List(context.Background(), repository.BalanceFilter{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if total < 2 || len(items) < 2 {
		t.Fatalf("expected old and new runs in list, got %d", total)
	}
	for _, item := range items {
		if item.FrozenManifest == nil {
			t.Fatalf("listed run #%d missing frozen manifest", item.ID)
		}
		if item.ID == recalculated.ID && (!item.EvidenceStale || len(item.StaleChanges) == 0) {
			t.Fatalf("list must flag stale recalculated run, got %+v", item)
		}
	}

	// 8. 审计链保留过期标记事件，便于复核员追溯变化来源。
	var staleAudits []model.AuditEvent
	if err := db.Where("action = ? AND entity_id = ?", "balance_run.evidence_stale", recalculated.ID).Find(&staleAudits).Error; err != nil {
		t.Fatalf("query stale audits: %v", err)
	}
	if len(staleAudits) == 0 {
		t.Fatal("expected audit trail for stale evidence marking")
	}
}

func TestEvidenceStaleGuardRejectsReviewAcceptanceBeforeStateMove(t *testing.T) {
	db := newFreezeTestDB(t)
	balanceService, measurementService, transferService := newFreezeServices(db)
	tank := seedFreezeTank(t, db)
	analyst := repository.Actor{UserID: 1, Email: "analyst@lng.local", Role: constants.RoleProcessAnalyst, RequestID: "req-guard-analyst"}
	reviewer := repository.Actor{UserID: 2, Email: "reviewer@lng.local", Role: constants.RoleReviewer, RequestID: "req-guard-reviewer"}

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-12 * time.Hour)
	periodEnd := now.Add(-30 * time.Minute)
	createFreezeSnapshot(t, measurementService, analyst, tank.ID, periodStart.Add(-time.Hour), 8.2, 0.3, constants.QualityGood)
	createFreezeSnapshot(t, measurementService, analyst, tank.ID, periodEnd, 8.1, 0.32, constants.QualityGood)
	run, err := balanceService.Run(context.Background(), dto.RunBalanceRequest{
		TankID: tank.ID, PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}, analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	submitted, err := balanceService.Submit(context.Background(), run.ID, dto.SubmitBalanceRequest{Version: run.Version}, analyst)
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	createFreezeTransfer(t, transferService, analyst, tank.ID, "inflow",
		periodStart.Add(2*time.Hour), periodStart.Add(3*time.Hour), 120000, "confirmed")
	if _, err := balanceService.Review(context.Background(), run.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "过期证据不应被接受",
	}, reviewer); err == nil {
		t.Fatal("acceptance must fail when frozen evidence no longer matches period")
	}
	finalRun, err := balanceService.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if finalRun.BalanceStatus != constants.BalancePendingReview || finalRun.ReviewedBy != nil {
		t.Fatalf("rejected review must leave pending run untouched: %+v", finalRun)
	}
	if !finalRun.EvidenceStale || len(finalRun.StaleChanges) != 1 {
		t.Fatalf("rejected review must persist stale reasons on old run: %+v", finalRun.StaleChanges)
	}
}
