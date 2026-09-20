import { Alert, Space, Tag, Tooltip, Typography } from 'antd'
import { Snowflake, AlertTriangle } from 'lucide-react'
import type { BalanceRun, FreezeChangeSource } from '../../types/balance'
import { dateTime } from '../../utils/format'

export const freezeStatusLabels: Record<'frozen' | 'stale', string> = {
  frozen: '证据冻结',
  stale: '证据过期'
}

const changeKindLabels: Record<string, string> = {
  opening_snapshot: '期初快照变化',
  closing_snapshot: '期末快照变化',
  snapshot_added: '期间新增快照',
  transfer_confirmed: '期间新增确认转移',
  transfer_unlocked: '已确认转移被取消'
}

// EvidenceFreezeTag 用于运行历史列表，紧凑表达冻结/过期状态。
export function EvidenceFreezeTag({ run }: { run: BalanceRun }) {
  if (!run.freeze_status) return null
  if (run.freeze_status === 'stale') {
    return (
      <Tooltip title={run.stale_reason || '证据已过期，必须重新计算'}>
        <Tag icon={<AlertTriangle size={12} />} color="warning">证据过期</Tag>
      </Tooltip>
    )
  }
  return (
    <Tooltip title={run.frozen_at ? `证据固化于 ${dateTime(run.frozen_at)}` : '证据冻结中'}>
      <Tag icon={<Snowflake size={12} />} color="cyan">证据冻结</Tag>
    </Tooltip>
  )
}

function ChangeSourceRow({ change }: { change: FreezeChangeSource }) {
  return (
    <li>
      <Space size={6} wrap>
        <Tag color={change.kind.includes('transfer') ? 'gold' : 'orange'}>
          {changeKindLabels[change.kind] ?? change.kind}
        </Tag>
        <Typography.Text type="secondary">
          {change.entity_type} #{change.entity_id}
          {change.occurred_at ? ` · ${dateTime(change.occurred_at)}` : ''}
        </Typography.Text>
      </Space>
      <div>{change.message}</div>
    </li>
  )
}

// EvidenceFreezeBanner 用于平衡详情：展示冻结状态、固化时间和过期变化来源，
// 并明确提示过期运行不得提交或复核、必须重新计算生成独立结果。
export function EvidenceFreezeBanner({ run }: { run: BalanceRun }) {
  if (!run.freeze_status) return null
  if (run.freeze_status === 'frozen') {
    return (
      <Alert
        className="freeze-banner"
        type="success"
        showIcon
        icon={<Snowflake size={16} />}
        message={
          <Space size={8} wrap>
            <strong>{freezeStatusLabels.frozen}</strong>
            <Typography.Text type="secondary">
              期初/期末快照与期间已确认转移摘要已固化
              {run.frozen_at ? ` · 固化于 ${dateTime(run.frozen_at)}` : ''}
              {run.freeze_checked_at ? ` · 最近核验 ${dateTime(run.freeze_checked_at)}` : ''}
            </Typography.Text>
          </Space>
        }
      />
    )
  }
  return (
    <Alert
      className="freeze-banner"
      type="warning"
      showIcon
      icon={<AlertTriangle size={16} />}
      message={
        <Space direction="vertical" size={4}>
          <Space size={8} wrap>
            <strong>{freezeStatusLabels.stale}</strong>
            <Typography.Text type="secondary">
              固化证据与当前数据不一致，该运行不得提交或复核；请重新计算生成独立结果，旧结果与审计保留。
            </Typography.Text>
          </Space>
          {run.stale_reason && <Typography.Text>{run.stale_reason}</Typography.Text>}
          {run.freeze_checked_at && (
            <Typography.Text type="secondary">最近核验：{dateTime(run.freeze_checked_at)}</Typography.Text>
          )}
          {run.freeze_changes?.length ? (
            <ul className="freeze-change-list">
              {run.freeze_changes.map((change, index) => (
                <ChangeSourceRow key={`${change.kind}-${change.entity_id}-${index}`} change={change} />
              ))}
            </ul>
          ) : null}
        </Space>
      }
    />
  )
}

// isStaleRun 供工作流按钮判断是否禁用提交/复核。
export function isStaleRun(run?: BalanceRun | null): boolean {
  return run?.freeze_status === 'stale'
}
