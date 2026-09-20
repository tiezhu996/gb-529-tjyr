package model

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/datatypes"

	"lng-boiloff-gas-balance/backend/internal/constants"
)

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
	Version            uint                     `json:"version" gorm:"not null;default:1"`
	CreatedBy          uint                     `json:"created_by" gorm:"not null"`
	ReviewedBy         *uint                    `json:"reviewed_by"`
	ReviewNote         string                   `json:"review_note" gorm:"size:1000"`
	ReviewedAt         *time.Time               `json:"reviewed_at"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	Tank               *StorageTank             `json:"tank,omitempty" gorm:"foreignKey:TankID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`

	// 以下字段不持久化在 balance_runs 表，由证据冻结核验结果填充后透出到列表与详情。
	FreezeStatus    EvidenceFreezeStatus `json:"freeze_status,omitempty" gorm:"-"`
	StaleReason     string               `json:"stale_reason,omitempty" gorm:"-"`
	FreezeChanges   []FreezeChangeSource `json:"freeze_changes,omitempty" gorm:"-"`
	FrozenAt        *time.Time           `json:"frozen_at,omitempty" gorm:"-"`
	FreezeCheckedAt *time.Time           `json:"freeze_checked_at,omitempty" gorm:"-"`
}

func (BalanceRun) TableName() string { return "balance_runs" }

// EvidenceFreezeStatus 描述平衡运行证据冻结摘要的生命周期。
// frozen: 计算时固化，当前证据仍然有效
// stale: 期间新增快照或确认转移后证据过期，必须重新计算
type EvidenceFreezeStatus string

const (
	EvidenceFreezeFrozen EvidenceFreezeStatus = "frozen"
	EvidenceFreezeStale  EvidenceFreezeStatus = "stale"
)

// 证据过期的变化来源类型，用于列表和详情向操作者解释重新计算原因。
const (
	FreezeSourceOpeningSnapshot   = "opening_snapshot"
	FreezeSourceClosingSnapshot   = "closing_snapshot"
	FreezeSourceSnapshotAdded     = "snapshot_added"
	FreezeSourceTransferConfirmed = "transfer_confirmed"
	FreezeSourceTransferUnlocked  = "transfer_unlocked"
)

// BalanceEvidenceFreeze 保存一次平衡运行计算时刻固化的证据摘要。
// 摘要只用于事后比对，不参与质量平衡方程；输入原值仍保存在 input_snapshot_json。
type BalanceEvidenceFreeze struct {
	ID             uint                 `json:"id" gorm:"primaryKey"`
	BalanceRunID   uint                 `json:"balance_run_id" gorm:"not null;uniqueIndex"`
	TankID         uint                 `json:"tank_id" gorm:"not null;index"`
	PeriodStart    time.Time            `json:"period_start" gorm:"not null"`
	PeriodEnd      time.Time            `json:"period_end" gorm:"not null"`
	FreezeStatus   EvidenceFreezeStatus `json:"freeze_status" gorm:"type:varchar(16);not null;default:frozen"`
	OpeningDigest  string               `json:"opening_digest" gorm:"size:64;not null"`
	ClosingDigest  string               `json:"closing_digest" gorm:"size:64;not null"`
	TransferDigest string               `json:"transfer_digest" gorm:"size:64;not null"`
	TransferCount  int                  `json:"transfer_count" gorm:"not null;default:0"`
	SnapshotCount  int                  `json:"snapshot_count" gorm:"not null;default:0"`
	SummaryJSON    datatypes.JSON       `json:"summary_json" gorm:"type:jsonb;not null"`
	StaleReason    string               `json:"stale_reason" gorm:"size:500;not null;default:''"`
	FrozenAt       time.Time            `json:"frozen_at" gorm:"not null"`
	CheckedAt      time.Time            `json:"checked_at" gorm:"not null"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	BalanceRun     *BalanceRun          `json:"balance_run,omitempty" gorm:"foreignKey:BalanceRunID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (BalanceEvidenceFreeze) TableName() string { return "balance_evidence_freezes" }

// FrozenSnapshotItem 是固化摘要中单条边界快照的可重放摘要。
type FrozenSnapshotItem struct {
	ID                        uint    `json:"id"`
	MeasuredAt                string  `json:"measured_at"`
	LiquidLevelM              float64 `json:"liquid_level_m"`
	LiquidTempC               float64 `json:"liquid_temp_c"`
	VaporPressureKPA          float64 `json:"vapor_pressure_kpa"`
	DensityKGM3               float64 `json:"density_kgm3"`
	CalculatedLiquidMassKG    float64 `json:"calculated_liquid_mass_kg"`
	MeasurementUncertaintyPct float64 `json:"measurement_uncertainty_pct"`
	QualityFlag               string  `json:"quality_flag"`
}

// FrozenTransferItem 是固化摘要中单条期间已确认转移的摘要。
type FrozenTransferItem struct {
	ID                        uint    `json:"id"`
	OperationType             string  `json:"operation_type"`
	StartAt                   string  `json:"start_at"`
	EndAt                     string  `json:"end_at"`
	MeasuredMassKG            float64 `json:"measured_mass_kg"`
	MeasurementUncertaintyPct float64 `json:"measurement_uncertainty_pct"`
	OperationStatus           string  `json:"operation_status"`
	CounterpartyRef           string  `json:"counterparty_ref"`
}

// EvidenceFreezeSummary 是计算时刻固化的完整证据摘要（JSON 形态）。
type EvidenceFreezeSummary struct {
	SchemaVersion      int                  `json:"schema_version"`
	FrozenAt           string               `json:"frozen_at"`
	TankID             uint                 `json:"tank_id"`
	PeriodStart        string               `json:"period_start"`
	PeriodEnd          string               `json:"period_end"`
	OpeningSnapshot    FrozenSnapshotItem   `json:"opening_snapshot"`
	ClosingSnapshot    FrozenSnapshotItem   `json:"closing_snapshot"`
	PeriodSnapshots    []FrozenSnapshotItem `json:"period_snapshots"`
	ConfirmedTransfers []FrozenTransferItem `json:"confirmed_transfers"`
}

// FreezeChangeSource 描述证据过期后检测到的单条变化来源。
type FreezeChangeSource struct {
	Kind       string `json:"kind"`
	EntityType string `json:"entity_type"`
	EntityID   uint   `json:"entity_id"`
	Message    string `json:"message"`
	OccurredAt string `json:"occurred_at,omitempty"`
}

// MustMarshalFreezeSummary 在已知摘要结构合法时序列化，供计算事务内调用。
func MustMarshalFreezeSummary(summary EvidenceFreezeSummary) datatypes.JSON {
	raw, err := json.Marshal(summary)
	if err != nil {
		panic(fmt.Errorf("marshal evidence freeze summary: %w", err))
	}
	return datatypes.JSON(raw)
}
