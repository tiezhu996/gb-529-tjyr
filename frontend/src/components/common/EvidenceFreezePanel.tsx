import { Alert, Descriptions, Table, Tag } from 'antd'
import { Snowflake, TriangleAlert } from 'lucide-react'
import type { BalanceRun, EvidenceChange } from '../../types/balance'
import { evidenceChangeLabels } from '../../types/balance'
import { dateTime } from '../../utils/format'

const shortDigest = (digest: string) => digest.slice(0, 12)

const changeTagColor = (change: EvidenceChange): string => {
  if (change.kind === 'evidence_freeze_missing') return 'error'
  if (change.kind.includes('snapshot')) return 'warning'
  return 'orange'
}

export function EvidenceFreezePanel({ run }: { run?: BalanceRun }) {
  if (!run) return null
  const manifest = run.frozen_manifest
  const changes = run.stale_changes ?? []
  return (
    <section className="freeze-panel" aria-label="证据冻结状态">
      <div className="section-heading">
        <div>
          <span className="eyebrow">FROZEN EVIDENCE CHAIN</span>
          <h2>证据冻结</h2>
        </div>
        {run.evidence_stale ? (
          <Tag className="status-tag" color="error" icon={<TriangleAlert size={13} />}>
            证据过期 · {changes.length} 项变化
          </Tag>
        ) : (
          <Tag className="status-tag" color="success" icon={<Snowflake size={13} />}>
            冻结一致
          </Tag>
        )}
      </div>
      {manifest ? (
        <>
          <Descriptions size="small" column={{ xs: 1, sm: 2, lg: 3 }} bordered>
            <Descriptions.Item label="冻结格式">{manifest.freeze_version}</Descriptions.Item>
            <Descriptions.Item label="冻结时间">{dateTime(manifest.frozen_at)}</Descriptions.Item>
            <Descriptions.Item label="期间转移摘要">
              <span className="mono">{shortDigest(manifest.transfers_digest)}</span>
            </Descriptions.Item>
            <Descriptions.Item label="期初快照">
              #{manifest.opening_snapshot.snapshot_id} · {dateTime(manifest.opening_snapshot.measured_at)} ·{' '}
              <span className="mono">{shortDigest(manifest.opening_snapshot.digest)}</span>
            </Descriptions.Item>
            <Descriptions.Item label="期末快照">
              #{manifest.closing_snapshot.snapshot_id} · {dateTime(manifest.closing_snapshot.measured_at)} ·{' '}
              <span className="mono">{shortDigest(manifest.closing_snapshot.digest)}</span>
            </Descriptions.Item>
            <Descriptions.Item label="已确认转移">{manifest.transfers.length} 条</Descriptions.Item>
          </Descriptions>
          {manifest.transfers.length > 0 && (
            <Table
              className="evidence-table"
              rowKey={(item) => 'frozen-transfer-' + item.transfer_id}
              size="small"
              pagination={false}
              dataSource={manifest.transfers}
              columns={[
                { title: '冻结转移', dataIndex: 'transfer_id', render: (value: number) => '#' + value },
                { title: '类型', dataIndex: 'operation_type', render: (value: string) => value === 'inflow' ? '流入' : '流出' },
                { title: '开始', dataIndex: 'start_at', render: dateTime },
                { title: '结束', dataIndex: 'end_at', render: dateTime },
                { title: '摘要', dataIndex: 'digest', render: (value: string) => <span className="mono">{shortDigest(value)}</span> }
              ]}
            />
          )}
        </>
      ) : (
        <Alert type="error" showIcon message="该运行缺少证据冻结清单，必须重新计算后才能提交或复核。" />
      )}
      {run.evidence_stale && (
        <Alert
          className="freeze-stale-alert"
          type="error"
          showIcon
          icon={<TriangleAlert size={16} />}
          message="证据已过期，旧结果仅作审计保留，请重新运行平衡生成独立结果。"
          description={
            <Table<EvidenceChange>
              rowKey={(item) => item.kind + '-' + item.entity_id}
              size="small"
              pagination={false}
              dataSource={changes}
              columns={[
                { title: '变化来源', dataIndex: 'kind', render: (value: keyof typeof evidenceChangeLabels) => <Tag color={changeTagColor(changes.find((c) => c.kind === value)!)}>{evidenceChangeLabels[value]}</Tag> },
                { title: '证据', key: 'entity', render: (_, item) => item.entity_id ? '#' + item.entity_id : '—' },
                { title: '说明', dataIndex: 'detail' },
                { title: '检测时间', dataIndex: 'changed_at', render: (value?: string) => value ? dateTime(value) : '实时' }
              ]}
            />
          }
        />
      )}
    </section>
  )
}
