package service

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/internal/repository"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

type freezeFixture struct {
	db          *gorm.DB
	balance     *BalanceService
	analyst     repository.Actor
	reviewer    repository.Actor
	tank        model.StorageTank
	periodStart time.Time
	periodEnd   time.Time
}

func newFreezeFixture(t *testing.T) freezeFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:freeze-evidence-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.StorageTank{}, &model.MeasurementSnapshot{},
		&model.TransferOperation{}, &model.BalanceRun{}, &model.BalanceEvidenceFreeze{}, &model.AuditEvent{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tank := model.StorageTank{
		TankCode: "TK-FRZ", Name: "冻结校验罐", NominalCapacityM3: 180000, MinLevelM: 0, MaxLevelM: 12,
		ReferenceDensityKGM3: 452, ReferenceTemperatureC: -160, ThermalExpansionPerC: 0.0035,
		CapacityCurveJSON: []byte(`{"coefficients":[0,15000]}`), CoefficientVersion: "CV-FRZ-1",
		TankStatus: "active", Version: 1,
	}
	if err := db.Create(&tank).Error; err != nil {
		t.Fatalf("create tank: %v", err)
	}
	now := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	start := now.Add(26 * time.Hour)
	end := start.Add(24 * time.Hour)
	balanceSvc := NewBalanceService(
		repository.NewBalanceRepository(db),
		repository.NewTankRepository(db),
		repository.NewMeasurementRepository(db),
		repository.NewTransferRepository(db),
	)
	return freezeFixture{
		db: db, balance: balanceSvc,
		analyst:  repository.Actor{UserID: 1, Email: "analyst@lng.local", Role: constants.RoleProcessAnalyst, RequestID: "req-analyst"},
		reviewer: repository.Actor{UserID: 2, Email: "reviewer@lng.local", Role: constants.RoleReviewer, RequestID: "req-reviewer"},
		tank:     tank, periodStart: start, periodEnd: end,
	}
}

func (f freezeFixture) snapshot(id uint, measuredAt time.Time, level, mass float64, quality constants.QualityFlag) model.MeasurementSnapshot {
	return model.MeasurementSnapshot{
		ID: id, TankID: f.tank.ID, MeasuredAt: measuredAt, LiquidLevelM: level, LiquidTempC: -160,
		VaporPressureKPA: 110, DensityKGM3: 451, CalculatedVolumeM3: level * 15000,
		TemperatureDensityKGM3: 451, CalculatedLiquidMassKG: mass,
		MeasurementUncertaintyPct: 0.3, QualityFlag: quality, SourceNote: "冻结校验测试快照", CreatedBy: 1,
	}
}

func (f freezeFixture) createSnapshot(t *testing.T, snapshot model.MeasurementSnapshot) {
	t.Helper()
	if err := f.db.Create(&snapshot).Error; err != nil {
		t.Fatalf("create snapshot #%d: %v", snapshot.ID, err)
	}
}

func (f freezeFixture) createTransfer(t *testing.T, transfer model.TransferOperation) {
	t.Helper()
	if err := f.db.Create(&transfer).Error; err != nil {
		t.Fatalf("create transfer #%d: %v", transfer.ID, err)
	}
}

func (f freezeFixture) runRequest() dto.RunBalanceRequest {
	start, end := f.periodStart, f.periodEnd
	return dto.RunBalanceRequest{TankID: f.tank.ID, PeriodStart: &start, PeriodEnd: &end}
}

func TestFreezeEvidenceStaleAfterSnapshotAddedBlocksSubmit(t *testing.T) {
	fixture := newFreezeFixture(t)
	ctx := context.Background()
	opening := fixture.snapshot(101, fixture.periodStart.Add(-2*time.Hour), 8.2, 55_000_000, constants.QualityGood)
	closing := fixture.snapshot(102, fixture.periodEnd.Add(-2*time.Hour), 8.1, 54_900_000, constants.QualityGood)
	fixture.createSnapshot(t, opening)
	fixture.createSnapshot(t, closing)

	run, err := fixture.balance.Run(ctx, fixture.runRequest(), fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	if run.FreezeStatus != model.EvidenceFreezeFrozen {
		t.Fatalf("new run freeze status = %q, want frozen", run.FreezeStatus)
	}

	// 期间新增一条快照（不可变，只能新建）：列表必须标记过期并给出来源。
	fixture.createSnapshot(t, fixture.snapshot(103, fixture.periodEnd.Add(-time.Hour), 8.05, 54_600_000, constants.QualityGood))
	items, _, err := fixture.balance.List(ctx, repository.BalanceFilter{Page: 1, PageSize: 20}, fixture.analyst)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(items) != 1 || items[0].FreezeStatus != model.EvidenceFreezeStale {
		t.Fatalf("expected single stale run, got %+v", items)
	}
	if len(items[0].FreezeChanges) == 0 {
		t.Fatal("stale run must list change sources")
	}
	if items[0].StaleReason == "" {
		t.Fatal("stale run must carry a human-readable reason")
	}
	found := false
	for _, change := range items[0].FreezeChanges {
		if change.Kind == model.FreezeSourceSnapshotAdded && change.EntityID == 103 {
			found = true
		}
	}
	if !found {
		t.Fatalf("changes must reference new snapshot #103: %+v", items[0].FreezeChanges)
	}

	// 过期运行不得提交复核；重新计算产生独立新结果，旧运行保留。
	_, err = fixture.balance.Submit(ctx, run.ID, dto.SubmitBalanceRequest{Version: run.Version}, fixture.analyst)
	if err == nil {
		t.Fatal("expected stale run submit to be rejected")
	}
	appErr, ok := err.(*api.Error)
	if !ok || appErr.Code != "BALANCE_EVIDENCE_STALE" {
		t.Fatalf("expected BALANCE_EVIDENCE_STALE, got %v", err)
	}

	recalculated, err := fixture.balance.Run(ctx, fixture.runRequest(), fixture.analyst)
	if err != nil {
		t.Fatalf("recalculate balance: %v", err)
	}
	if recalculated.ID == run.ID {
		t.Fatal("recalculation must create an independent run, not overwrite the stale one")
	}
	if recalculated.FreezeStatus != model.EvidenceFreezeFrozen {
		t.Fatalf("recalculated run must be frozen, got %q", recalculated.FreezeStatus)
	}
	var oldCount int64
	if err := fixture.db.Model(&model.BalanceRun{}).Where("id = ?", run.ID).Count(&oldCount).Error; err != nil || oldCount != 1 {
		t.Fatalf("stale run and its audit trail must be retained, count=%d err=%v", oldCount, err)
	}
	var freezeCount int64
	if err := fixture.db.Model(&model.BalanceEvidenceFreeze{}).Count(&freezeCount).Error; err != nil {
		t.Fatalf("count freezes: %v", err)
	}
	if freezeCount != 2 {
		t.Fatalf("expected two independent freeze records, got %d", freezeCount)
	}
}

func TestFreezeEvidenceStaleAfterTransferConfirmedBlocksReview(t *testing.T) {
	fixture := newFreezeFixture(t)
	ctx := context.Background()
	fixture.createSnapshot(t, fixture.snapshot(201, fixture.periodStart.Add(-2*time.Hour), 8.2, 55_000_000, constants.QualityGood))
	fixture.createSnapshot(t, fixture.snapshot(202, fixture.periodEnd.Add(-3*time.Hour), 8.1, 54_700_000, constants.QualityGood))
	draftTransfer := model.TransferOperation{
		ID: 301, TankID: fixture.tank.ID, OperationType: "inflow",
		StartAt: fixture.periodStart.Add(2 * time.Hour), EndAt: fixture.periodStart.Add(3 * time.Hour),
		MeasuredMassKG: 200000, MeasurementUncertaintyPct: 0.25, CounterpartyRef: "DRAFT-METER",
		OperationStatus: "draft", Version: 1, CreatedBy: 1,
	}
	fixture.createTransfer(t, draftTransfer)

	run, err := fixture.balance.Run(ctx, fixture.runRequest(), fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	submitted, err := fixture.balance.Submit(ctx, run.ID, dto.SubmitBalanceRequest{Version: run.Version}, fixture.analyst)
	if err != nil {
		t.Fatalf("submit fresh run: %v", err)
	}

	// 期间内的草稿转移获得确认，已确认转移摘要变化，证据过期。
	if err := fixture.db.Model(&model.TransferOperation{}).Where("id = ?", draftTransfer.ID).
		Updates(map[string]any{"operation_status": "confirmed", "version": 2, "updated_at": time.Now().UTC()}).Error; err != nil {
		t.Fatalf("confirm transfer: %v", err)
	}
	detail, err := fixture.balance.Get(ctx, run.ID, fixture.reviewer)
	if err != nil {
		t.Fatalf("get run detail: %v", err)
	}
	if detail.FreezeStatus != model.EvidenceFreezeStale {
		t.Fatalf("expected stale after transfer confirmation, got %q", detail.FreezeStatus)
	}
	kinds := map[string]bool{}
	for _, change := range detail.FreezeChanges {
		kinds[change.Kind] = true
	}
	if !kinds[model.FreezeSourceTransferConfirmed] {
		t.Fatalf("expected transfer_confirmed change source, got %+v", detail.FreezeChanges)
	}

	// 复核事务与摘要检查同事务：过期运行不允许接受。
	_, err = fixture.balance.Review(ctx, submitted.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "证据过期也应被拒绝的复核",
	}, fixture.reviewer)
	if err == nil {
		t.Fatal("expected review of stale run to be rejected")
	}
	appErr, ok := err.(*api.Error)
	if !ok || appErr.Code != "BALANCE_EVIDENCE_STALE" {
		t.Fatalf("expected BALANCE_EVIDENCE_STALE on review, got %v", err)
	}

	// 作废仍允许对过期运行执行，旧结果与审计保留。
	if _, err := fixture.balance.Invalidate(ctx, run.ID, dto.InvalidateBalanceRequest{
		Version: submitted.Version, Reason: "证据过期，按管理程序作废旧结果",
	}, repository.Actor{UserID: 3, Email: "admin@lng.local", Role: constants.RoleAdmin, RequestID: "req-admin"}); err != nil {
		t.Fatalf("invalidate stale run: %v", err)
	}
	var status string
	if err := fixture.db.Model(&model.BalanceRun{}).Select("balance_status").Where("id = ?", run.ID).Scan(&status).Error; err != nil {
		t.Fatalf("reload status: %v", err)
	}
	if status != string(constants.BalanceInvalidated) {
		t.Fatalf("status = %q, want invalidated", status)
	}
}

func TestFreezeEvidenceStaysFrozenWhenUnrelatedDataChanges(t *testing.T) {
	fixture := newFreezeFixture(t)
	ctx := context.Background()
	fixture.createSnapshot(t, fixture.snapshot(401, fixture.periodStart.Add(-2*time.Hour), 8.2, 55_000_000, constants.QualityGood))
	fixture.createSnapshot(t, fixture.snapshot(402, fixture.periodEnd.Add(-2*time.Hour), 8.1, 54_900_000, constants.QualityGood))
	run, err := fixture.balance.Run(ctx, fixture.runRequest(), fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}

	// 更早期、不改变期初选择的快照不应使证据过期（懒核验自愈保守标记）。
	fixture.createSnapshot(t, fixture.snapshot(403, fixture.periodStart.Add(-20*time.Hour), 8.4, 56_200_000, constants.QualityGood))
	detail, err := fixture.balance.Get(ctx, run.ID, fixture.analyst)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if detail.FreezeStatus != model.EvidenceFreezeFrozen {
		t.Fatalf("expected frozen after unrelated snapshot, got %q (%s)", detail.FreezeStatus, detail.StaleReason)
	}
}
