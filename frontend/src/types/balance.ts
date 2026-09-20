import type { DeviationLevel } from './deviation'
import type { StorageTank } from './tank'

export type BalanceStatus = 'queued' | 'calculating' | 'pending_review' | 'accepted' | 'rejected' | 'invalidated'

export type EvidenceChangeKind =
  | 'opening_snapshot_changed'
  | 'closing_snapshot_changed'
  | 'snapshot_added'
  | 'snapshot_removed'
  | 'confirmed_transfer_added'
  | 'confirmed_transfer_removed'
  | 'confirmed_transfer_changed'
  | 'evidence_freeze_missing'

export interface EvidenceChange {
  kind: EvidenceChangeKind
  entity_id: number
  detail: string
  changed_at?: string
}

export interface FrozenSnapshotSummary {
  snapshot_id: number
  measured_at: string
  quality_flag: string
  level_m: number
  liquid_mass_kg: number
  digest: string
}

export interface FrozenTransferSummary {
  transfer_id: number
  operation_type: string
  start_at: string
  end_at: string
  mass_kg: number
  uncertainty_pct: number
  digest: string
}

export interface FrozenEvidenceManifest {
  freeze_version: string
  frozen_at: string
  period_start: string
  period_end: string
  opening_snapshot: FrozenSnapshotSummary
  closing_snapshot: FrozenSnapshotSummary
  transfers: FrozenTransferSummary[]
  transfers_digest: string
}

export const evidenceChangeLabels: Record<EvidenceChangeKind, string> = {
  opening_snapshot_changed: '期初快照变化',
  closing_snapshot_changed: '期末快照变化',
  snapshot_added: '期间新增快照',
  snapshot_removed: '边界快照缺失',
  confirmed_transfer_added: '新增已确认转移',
  confirmed_transfer_removed: '已确认转移移除',
  confirmed_transfer_changed: '已确认转移变化',
  evidence_freeze_missing: '缺少证据冻结'
}

export interface BalanceRun {
  id: number
  tank_id: number
  period_start: string
  period_end: string
  balance_status: BalanceStatus
  input_snapshot_json: Record<string, unknown>
  opening_mass_kg: number
  closing_mass_kg: number
  net_transfer_kg: number
  estimated_bog_kg: number
  uncertainty_kg: number
  interval_lower_kg: number
  interval_upper_kg: number
  deviation_pct: number
  deviation_level: DeviationLevel
  evidence_json: BalanceEvidence
  coefficient_version: string
  evidence_stale: boolean
  frozen_manifest?: FrozenEvidenceManifest | null
  stale_changes?: EvidenceChange[]
  version: number
  created_by: number
  reviewed_by?: number
  review_note: string
  reviewed_at?: string
  created_at: string
  updated_at: string
  tank?: StorageTank
}

export interface UncertaintyComponent {
  source: string
  entity_id: number
  mass_kg: number
  uncertainty_pct: number
  absolute_kg: number
}

export interface UncertaintyBreakdown {
  balance_run_id?: number
  combined_kg: number
  lower_kg: number
  upper_kg: number
  relationship: DeviationLevel
  components: UncertaintyComponent[]
}

export interface BalanceEvidence {
  algorithm_version?: string
  equation?: Record<string, number>
  uncertainty?: UncertaintyBreakdown
  safety_boundary?: string
}

export interface BalanceRunInput {
  tank_id: number
  period_start: string
  period_end: string
}
