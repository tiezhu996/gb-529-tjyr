package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"

	"lng-boiloff-gas-balance/backend/internal/constants"
)

// EvidenceFreezeV1 标记期初/期末快照与期间已确认转移摘要的冻结格式版本。
const EvidenceFreezeV1 = "evidence-freeze-v1"

// 证据差异来源类别，随冻结清单与列表/详情响应一并暴露给前后端。
const (
	EvidenceChangeOpeningChanged  = "opening_snapshot_changed"
	EvidenceChangeClosingChanged  = "closing_snapshot_changed"
	EvidenceChangeSnapshotAdded   = "snapshot_added"
	EvidenceChangeSnapshotRemoved = "snapshot_removed"
	EvidenceChangeTransferAdded   = "confirmed_transfer_added"
	EvidenceChangeTransferRemoved = "confirmed_transfer_removed"
	EvidenceChangeTransferChanged = "confirmed_transfer_changed"
	EvidenceChangeFreezeMissing   = "evidence_freeze_missing"
)

// EvidenceChange 描述冻结证据与当前期间证据之间的一条差异来源。
type EvidenceChange struct {
	Kind      string `json:"kind"`
	EntityID  uint   `json:"entity_id"`
	Detail    string `json:"detail"`
	ChangedAt string `json:"changed_at,omitempty"`
}

// FrozenSnapshotSummary 是计算时固化的单条边界快照摘要。
type FrozenSnapshotSummary struct {
	SnapshotID  uint    `json:"snapshot_id"`
	MeasuredAt  string  `json:"measured_at"`
	QualityFlag string  `json:"quality_flag"`
	LevelM      float64 `json:"level_m"`
	LiquidMass  float64 `json:"liquid_mass_kg"`
	Digest      string  `json:"digest"`
}

// FrozenTransferSummary 是计算时固化的单条已确认转移摘要。
type FrozenTransferSummary struct {
	TransferID     uint    `json:"transfer_id"`
	OperationType  string  `json:"operation_type"`
	StartAt        string  `json:"start_at"`
	EndAt          string  `json:"end_at"`
	MassKG         float64 `json:"mass_kg"`
	UncertaintyPct float64 `json:"uncertainty_pct"`
	Digest         string  `json:"digest"`
}

// FrozenEvidenceManifest 是随平衡结果保存的证据冻结清单。
type FrozenEvidenceManifest struct {
	FreezeVersion   string                  `json:"freeze_version"`
	FrozenAt        string                  `json:"frozen_at"`
	PeriodStart     string                  `json:"period_start"`
	PeriodEnd       string                  `json:"period_end"`
	OpeningSnapshot FrozenSnapshotSummary   `json:"opening_snapshot"`
	ClosingSnapshot FrozenSnapshotSummary   `json:"closing_snapshot"`
	Transfers       []FrozenTransferSummary `json:"transfers"`
	TransfersDigest string                  `json:"transfers_digest"`
}

type canonicalSnapshot struct {
	ID         uint    `json:"id"`
	MeasuredAt string  `json:"measured_at"`
	Quality    string  `json:"quality_flag"`
	LevelM     float64 `json:"level_m"`
	TempC      float64 `json:"liquid_temp_c"`
	Pressure   float64 `json:"vapor_pressure_kpa"`
	Density    float64 `json:"density_kgm3"`
	MassKG     float64 `json:"liquid_mass_kg"`
	UncertPct  float64 `json:"uncertainty_pct"`
}

type canonicalTransfer struct {
	ID           uint    `json:"id"`
	Type         string  `json:"type"`
	StartAt      string  `json:"start_at"`
	EndAt        string  `json:"end_at"`
	MassKG       float64 `json:"mass_kg"`
	UncertPct    float64 `json:"uncertainty_pct"`
	Counterparty string  `json:"counterparty_ref"`
	Status       string  `json:"status"`
}

func digestCanonical(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize evidence digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// snapshotCanonical 固化影响边界质量的全部计量字段，原值不可变因此可直接重放比对。
func snapshotCanonical(snapshot MeasurementSnapshot) canonicalSnapshot {
	return canonicalSnapshot{
		ID:         snapshot.ID,
		MeasuredAt: snapshot.MeasuredAt.UTC().Format(time.RFC3339Nano),
		Quality:    string(snapshot.QualityFlag),
		LevelM:     snapshot.LiquidLevelM,
		TempC:      snapshot.LiquidTempC,
		Pressure:   snapshot.VaporPressureKPA,
		Density:    snapshot.DensityKGM3,
		MassKG:     snapshot.CalculatedLiquidMassKG,
		UncertPct:  snapshot.MeasurementUncertaintyPct,
	}
}

func transferCanonical(transfer TransferOperation) canonicalTransfer {
	return canonicalTransfer{
		ID:           transfer.ID,
		Type:         transfer.OperationType,
		StartAt:      transfer.StartAt.UTC().Format(time.RFC3339Nano),
		EndAt:        transfer.EndAt.UTC().Format(time.RFC3339Nano),
		MassKG:       transfer.MeasuredMassKG,
		UncertPct:    transfer.MeasurementUncertaintyPct,
		Counterparty: transfer.CounterpartyRef,
		Status:       transfer.OperationStatus,
	}
}

// FrozenSnapshotSummaryOf 生成单条边界快照的冻结摘要与 SHA-256 指纹。
func FrozenSnapshotSummaryOf(snapshot MeasurementSnapshot) (FrozenSnapshotSummary, error) {
	digest, err := digestCanonical(snapshotCanonical(snapshot))
	if err != nil {
		return FrozenSnapshotSummary{}, err
	}
	return FrozenSnapshotSummary{
		SnapshotID:  snapshot.ID,
		MeasuredAt:  snapshot.MeasuredAt.UTC().Format(time.RFC3339Nano),
		QualityFlag: string(snapshot.QualityFlag),
		LevelM:      snapshot.LiquidLevelM,
		LiquidMass:  snapshot.CalculatedLiquidMassKG,
		Digest:      digest,
	}, nil
}

// FrozenTransferSummaryOf 生成单条已确认转移的冻结摘要与 SHA-256 指纹。
func FrozenTransferSummaryOf(transfer TransferOperation) (FrozenTransferSummary, error) {
	digest, err := digestCanonical(transferCanonical(transfer))
	if err != nil {
		return FrozenTransferSummary{}, err
	}
	return FrozenTransferSummary{
		TransferID:     transfer.ID,
		OperationType:  transfer.OperationType,
		StartAt:        transfer.StartAt.UTC().Format(time.RFC3339Nano),
		EndAt:          transfer.EndAt.UTC().Format(time.RFC3339Nano),
		MassKG:         transfer.MeasuredMassKG,
		UncertaintyPct: transfer.MeasurementUncertaintyPct,
		Digest:         digest,
	}, nil
}

// BuildFrozenManifest 在计算事务内固化边界快照与期间已确认转移的摘要指纹。
func BuildFrozenManifest(opening, closing MeasurementSnapshot, transfers []TransferOperation, periodStart, periodEnd, frozenAt time.Time) (FrozenEvidenceManifest, error) {
	openingSummary, err := FrozenSnapshotSummaryOf(opening)
	if err != nil {
		return FrozenEvidenceManifest{}, err
	}
	closingSummary, err := FrozenSnapshotSummaryOf(closing)
	if err != nil {
		return FrozenEvidenceManifest{}, err
	}
	summaries := make([]FrozenTransferSummary, 0, len(transfers))
	digests := make([]string, 0, len(transfers))
	for _, transfer := range transfers {
		summary, err := FrozenTransferSummaryOf(transfer)
		if err != nil {
			return FrozenEvidenceManifest{}, err
		}
		summaries = append(summaries, summary)
		digests = append(digests, summary.Digest)
	}
	sort.Strings(digests)
	overallDigest, err := digestCanonical(digests)
	if err != nil {
		return FrozenEvidenceManifest{}, err
	}
	return FrozenEvidenceManifest{
		FreezeVersion:   EvidenceFreezeV1,
		FrozenAt:        frozenAt.UTC().Format(time.RFC3339Nano),
		PeriodStart:     periodStart.UTC().Format(time.RFC3339Nano),
		PeriodEnd:       periodEnd.UTC().Format(time.RFC3339Nano),
		OpeningSnapshot: openingSummary,
		ClosingSnapshot: closingSummary,
		Transfers:       summaries,
		TransfersDigest: overallDigest,
	}, nil
}

// MarshalFrozenManifest 序列化冻结清单，供计算编排层随结果同事务保存。
func MarshalFrozenManifest(manifest FrozenEvidenceManifest) ([]byte, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal frozen evidence manifest: %w", err)
	}
	return raw, nil
}

// ParseFrozenManifest 解析冻结清单，第二个返回值表示该运行是否携带有效冻结。
func ParseFrozenManifest(raw []byte) (FrozenEvidenceManifest, bool, error) {
	var manifest FrozenEvidenceManifest
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "{}" {
		return manifest, false, nil
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return FrozenEvidenceManifest{}, false, fmt.Errorf("decode frozen evidence manifest: %w", err)
	}
	valid := manifest.FreezeVersion != "" && manifest.OpeningSnapshot.Digest != "" && manifest.ClosingSnapshot.Digest != ""
	return manifest, valid, nil
}

func snapshotChange(kind, label string, fallbackID uint, current *MeasurementSnapshot, changedAt time.Time) EvidenceChange {
	id := fallbackID
	if current != nil {
		id = current.ID
	}
	return EvidenceChange{
		Kind:      kind,
		EntityID:  id,
		Detail:    fmt.Sprintf("%s #%d", label, id),
		ChangedAt: changedAt.UTC().Format(time.RFC3339Nano),
	}
}

func transferChange(kind string, frozenID uint, current *TransferOperation, added bool, changedAt time.Time) EvidenceChange {
	change := EvidenceChange{Kind: kind, ChangedAt: changedAt.UTC().Format(time.RFC3339Nano)}
	if current != nil {
		change.EntityID = current.ID
		switch {
		case added:
			change.Detail = fmt.Sprintf("期间新增已确认转移 #%d", current.ID)
		case kind == EvidenceChangeTransferChanged:
			change.Detail = fmt.Sprintf("期间已确认转移 #%d 计量或状态已变化", current.ID)
		default:
			change.Detail = fmt.Sprintf("期间已确认转移 #%d", current.ID)
		}
	} else {
		change.EntityID = frozenID
		change.Detail = fmt.Sprintf("期间已确认转移 #%d 已不在确认集合中", frozenID)
	}
	return change
}

// DiffFrozenEvidence 比较冻结清单与当前边界证据，稳定排序后返回全部变化来源。
// 快照原值不可覆盖，因此同 ID 指纹变化必然来自边界选择切换或质量标记差异。
func DiffFrozenEvidence(manifest FrozenEvidenceManifest, opening, closing *MeasurementSnapshot, transfers []TransferOperation, changedAt time.Time) ([]EvidenceChange, error) {
	changes := make([]EvidenceChange, 0)
	if opening == nil {
		changes = append(changes, snapshotChange(EvidenceChangeSnapshotRemoved, "期初快照缺失，原期初快照", manifest.OpeningSnapshot.SnapshotID, nil, changedAt))
	} else if summary, err := FrozenSnapshotSummaryOf(*opening); err != nil {
		return nil, err
	} else if summary.Digest != manifest.OpeningSnapshot.Digest {
		kind := EvidenceChangeOpeningChanged
		if opening.ID != manifest.OpeningSnapshot.SnapshotID {
			kind = EvidenceChangeSnapshotAdded
		}
		changes = append(changes, snapshotChange(kind, "期初快照已变化，当前期初快照", manifest.OpeningSnapshot.SnapshotID, opening, changedAt))
	}
	if closing == nil {
		changes = append(changes, snapshotChange(EvidenceChangeSnapshotRemoved, "期末快照缺失，原期末快照", manifest.ClosingSnapshot.SnapshotID, nil, changedAt))
	} else if summary, err := FrozenSnapshotSummaryOf(*closing); err != nil {
		return nil, err
	} else if summary.Digest != manifest.ClosingSnapshot.Digest {
		kind := EvidenceChangeClosingChanged
		if closing.ID != manifest.ClosingSnapshot.SnapshotID {
			kind = EvidenceChangeSnapshotAdded
		}
		changes = append(changes, snapshotChange(kind, "期末快照已变化，当前期末快照", manifest.ClosingSnapshot.SnapshotID, closing, changedAt))
	}
	currentByID := make(map[uint]TransferOperation, len(transfers))
	digests := make([]string, 0, len(transfers))
	for _, transfer := range transfers {
		currentByID[transfer.ID] = transfer
		summary, err := FrozenTransferSummaryOf(transfer)
		if err != nil {
			return nil, err
		}
		digests = append(digests, summary.Digest)
	}
	frozenByID := make(map[uint]FrozenTransferSummary, len(manifest.Transfers))
	for _, frozen := range manifest.Transfers {
		frozenByID[frozen.TransferID] = frozen
	}
	for _, transfer := range transfers {
		frozen, exists := frozenByID[transfer.ID]
		summary, err := FrozenTransferSummaryOf(transfer)
		if err != nil {
			return nil, err
		}
		switch {
		case !exists:
			changes = append(changes, transferChange(EvidenceChangeTransferAdded, 0, &transfer, true, changedAt))
		case frozen.Digest != summary.Digest:
			changes = append(changes, transferChange(EvidenceChangeTransferChanged, transfer.ID, &transfer, false, changedAt))
		}
	}
	for id := range frozenByID {
		if _, exists := currentByID[id]; !exists {
			changes = append(changes, transferChange(EvidenceChangeTransferRemoved, id, nil, false, changedAt))
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Kind != changes[j].Kind {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].EntityID < changes[j].EntityID
	})
	sort.Strings(digests)
	overallDigest, err := digestCanonical(digests)
	if err != nil {
		return nil, err
	}
	if overallDigest != manifest.TransfersDigest && len(changes) == 0 {
		changes = append(changes, EvidenceChange{
			Kind:      EvidenceChangeTransferChanged,
			Detail:    "期间已确认转移集合摘要不一致",
			ChangedAt: changedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return changes, nil
}

type BalanceRun struct {
	ID                 uint                     `json:"id" gorm:"primaryKey"`
	TankID             uint                     `json:"tank_id" gorm:"not null;index"`
	PeriodStart        time.Time                `json:"period_start" gorm:"not null;index"`
	PeriodEnd          time.Time                `json:"period_end" gorm:"not null;index"`
	BalanceStatus      constants.BalanceStatus  `json:"balance_status" gorm:"type:varchar(24);not null;check:balance_status IN ('queued','calculating','pending_review','accepted','rejected','invalidated')"`
	InputSnapshotJSON  datatypes.JSON           `json:"input_snapshot_json" gorm:"type:jsonb;not null"`
	OpeningMassKG      float64                  `json:"opening_mass_kg" gorm:"not null"`
	ClosingMassKG      float64                  `json:"closing_mass_kg" gorm:"not null"`
	NetTransferKG      float64                  `json:"net_transfer_kg" gorm:"not null"`
	EstimatedBOGKG     float64                  `json:"estimated_bog_kg" gorm:"column:estimated_bog_kg;not null"`
	UncertaintyKG      float64                  `json:"uncertainty_kg" gorm:"not null"`
	IntervalLowerKG    float64                  `json:"interval_lower_kg" gorm:"not null"`
	IntervalUpperKG    float64                  `json:"interval_upper_kg" gorm:"not null"`
	DeviationPct       float64                  `json:"deviation_pct" gorm:"not null"`
	DeviationLevel     constants.DeviationLevel `json:"deviation_level" gorm:"type:varchar(24);not null"`
	EvidenceJSON       datatypes.JSON           `json:"evidence_json" gorm:"type:jsonb;not null"`
	CoefficientVersion string                   `json:"coefficient_version" gorm:"size:32;not null"`
	FrozenEvidenceJSON datatypes.JSON           `json:"-" gorm:"type:jsonb;not null;default:'{}'"`
	EvidenceStale      bool                     `json:"evidence_stale" gorm:"not null;default:false"`
	StaleReasonsJSON   datatypes.JSON           `json:"-" gorm:"type:jsonb;not null;default:'[]'"`
	Version            uint                     `json:"version" gorm:"not null;default:1"`
	CreatedBy          uint                     `json:"created_by" gorm:"not null"`
	ReviewedBy         *uint                    `json:"reviewed_by"`
	ReviewNote         string                   `json:"review_note" gorm:"size:1000"`
	ReviewedAt         *time.Time               `json:"reviewed_at"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	Tank               *StorageTank             `json:"tank,omitempty" gorm:"foreignKey:TankID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`

	// FrozenManifest 与 StaleChanges 只在读取时实时校验填充，不写入数据库。
	FrozenManifest *FrozenEvidenceManifest `json:"frozen_manifest,omitempty" gorm:"-"`
	StaleChanges   []EvidenceChange        `json:"stale_changes,omitempty" gorm:"-"`
}

func (BalanceRun) TableName() string { return "balance_runs" }
