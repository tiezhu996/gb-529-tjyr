package model

import (
	"testing"
	"time"

	"lng-boiloff-gas-balance/backend/internal/constants"
)

func freezeTestClock() time.Time {
	return time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
}

func freezeTestSnapshots() (MeasurementSnapshot, MeasurementSnapshot) {
	opening := MeasurementSnapshot{
		ID: 10, TankID: 1, MeasuredAt: freezeTestClock().Add(-time.Hour),
		LiquidLevelM: 8.2, LiquidTempC: -160.2, VaporPressureKPA: 111, DensityKGM3: 451.8,
		CalculatedLiquidMassKG: 55_000_000, MeasurementUncertaintyPct: 0.3, QualityFlag: constants.QualityGood,
	}
	closing := MeasurementSnapshot{
		ID: 11, TankID: 1, MeasuredAt: freezeTestClock().Add(23 * time.Hour),
		LiquidLevelM: 8.14, LiquidTempC: -159.9, VaporPressureKPA: 113, DensityKGM3: 451.2,
		CalculatedLiquidMassKG: 54_900_000, MeasurementUncertaintyPct: 0.32, QualityFlag: constants.QualityGood,
	}
	return opening, closing
}

func freezeTestTransfers() []TransferOperation {
	return []TransferOperation{
		{
			ID: 20, TankID: 1, OperationType: "inflow",
			StartAt: freezeTestClock().Add(4 * time.Hour), EndAt: freezeTestClock().Add(5 * time.Hour),
			MeasuredMassKG: 250000, MeasurementUncertaintyPct: 0.2,
			CounterpartyRef: "JETTY-A-01", OperationStatus: "confirmed",
		},
	}
}

func TestDiffFrozenEvidenceDetectsAddedConfirmedTransfer(t *testing.T) {
	opening, closing := freezeTestSnapshots()
	transfers := freezeTestTransfers()
	start, end := freezeTestClock(), freezeTestClock().Add(24*time.Hour)
	manifest, err := BuildFrozenManifest(opening, closing, transfers, start, end, freezeTestClock())
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	changes, err := DiffFrozenEvidence(manifest, &opening, &closing, transfers, freezeTestClock())
	if err != nil {
		t.Fatalf("diff unchanged evidence: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("unchanged evidence must stay frozen, got %d changes: %+v", len(changes), changes)
	}
	added := append(transfers, TransferOperation{
		ID: 21, TankID: 1, OperationType: "outflow",
		StartAt: freezeTestClock().Add(8 * time.Hour), EndAt: freezeTestClock().Add(9 * time.Hour),
		MeasuredMassKG: 90000, MeasurementUncertaintyPct: 0.25,
		CounterpartyRef: "SENDOUT-02", OperationStatus: "confirmed",
	})
	changes, err = DiffFrozenEvidence(manifest, &opening, &closing, added, freezeTestClock())
	if err != nil {
		t.Fatalf("diff added transfer: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != EvidenceChangeTransferAdded || changes[0].EntityID != 21 {
		t.Fatalf("expected single added-transfer change, got %+v", changes)
	}
}

func TestDiffFrozenEvidenceDetectsBoundarySnapshotReplacement(t *testing.T) {
	opening, closing := freezeTestSnapshots()
	transfers := freezeTestTransfers()
	start, end := freezeTestClock(), freezeTestClock().Add(24*time.Hour)
	manifest, err := BuildFrozenManifest(opening, closing, transfers, start, end, freezeTestClock())
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	newClosing := closing
	newClosing.ID = 12
	newClosing.MeasuredAt = freezeTestClock().Add(23*time.Hour + 30*time.Minute)
	newClosing.CalculatedLiquidMassKG = 54_875_000
	changes, err := DiffFrozenEvidence(manifest, &opening, &newClosing, transfers, freezeTestClock())
	if err != nil {
		t.Fatalf("diff closing replacement: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected one closing change, got %+v", changes)
	}
	if changes[0].Kind != EvidenceChangeSnapshotAdded || changes[0].EntityID != 12 {
		t.Fatalf("expected added snapshot change for new closing, got %+v", changes[0])
	}

	mutatedOpening := opening
	mutatedOpening.QualityFlag = constants.QualitySuspect
	changes, err = DiffFrozenEvidence(manifest, &mutatedOpening, &closing, transfers, freezeTestClock())
	if err != nil {
		t.Fatalf("diff opening mutation: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != EvidenceChangeOpeningChanged {
		t.Fatalf("expected opening digest change, got %+v", changes)
	}
}

func TestDiffFrozenEvidenceDetectsRemovedTransfer(t *testing.T) {
	opening, closing := freezeTestSnapshots()
	transfers := freezeTestTransfers()
	start, end := freezeTestClock(), freezeTestClock().Add(24*time.Hour)
	manifest, err := BuildFrozenManifest(opening, closing, transfers, start, end, freezeTestClock())
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	changes, err := DiffFrozenEvidence(manifest, &opening, &closing, nil, freezeTestClock())
	if err != nil {
		t.Fatalf("diff removed transfer: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != EvidenceChangeTransferRemoved || changes[0].EntityID != 20 {
		t.Fatalf("expected removed transfer change, got %+v", changes)
	}
}
