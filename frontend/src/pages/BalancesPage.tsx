import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Form, Input, Modal, Select, Table, Tag, Tooltip } from 'antd'
import { CheckCircle2, Play, RefreshCw, Send, Snowflake, TriangleAlert, XCircle } from 'lucide-react'
import { EvidenceBreakdownPanel } from '../components/common/EvidenceBreakdownPanel'
import { EvidenceFreezePanel } from '../components/common/EvidenceFreezePanel'
import { MassBalanceWaterfall } from '../components/common/MassBalanceWaterfall'
import { PageHeader } from '../components/common/PageHeader'
import { useAuth } from '../hooks/useAuth'
import { useBalanceRun } from '../hooks/useBalanceRun'
import { useTankStore } from '../stores/tankStore'
import type { BalanceRun, BalanceRunInput, BalanceStatus } from '../types/balance'
import { deviationLabels } from '../types/deviation'
import { dateTime, kg, localInputDate, number } from '../utils/format'

const statusLabels: Record<BalanceStatus, string> = {
  queued: '排队', calculating: '已计算', pending_review: '待复核',
  accepted: '已接受', rejected: '已驳回', invalidated: '已作废'
}

export function BalancesPage() {
  const { user, can } = useAuth()
  const store = useBalanceRun()
  const tanks = useTankStore()
  const [runOpen, setRunOpen] = useState(false)
  const [reviewTarget, setReviewTarget] = useState<'accepted' | 'rejected'>('accepted')
  const [reviewOpen, setReviewOpen] = useState(false)
  const [runForm] = Form.useForm<BalanceRunInput>()
  const [reviewForm] = Form.useForm<{ note: string }>()
  useEffect(() => { void Promise.all([store.load(), tanks.load()]) }, [])
  const selected = useMemo(() => store.items.find((item) => item.id === store.selectedId) ?? store.items[0], [store.items, store.selectedId])
  const openRun = () => {
    runForm.setFieldsValue({ tank_id: tanks.items[0]?.id })
    setRunOpen(true)
  }
  const run = async (values: BalanceRunInput) => {
    await store.run({
      tank_id: values.tank_id,
      period_start: new Date(values.period_start).toISOString(),
      period_end: new Date(values.period_end).toISOString()
    })
    setRunOpen(false)
  }
  const review = async ({ note }: { note: string }) => {
    if (!selected) return
    await store.review(selected, reviewTarget, note)
    setReviewOpen(false)
    reviewForm.resetFields()
  }
  const openReview = (target: 'accepted' | 'rejected') => {
    setReviewTarget(target)
    setReviewOpen(true)
  }
  return (
    <>
      <PageHeader
        eyebrow="MASS BALANCE WORKBENCH"
        title="平衡工作台"
        description="重放期初、物理转移和期末证据，输出 BOG / 未解释项及其不确定度关系。"
        actions={
          <>
            <Button icon={<RefreshCw size={16} />} onClick={() => void store.load()}>刷新</Button>
            {can('process_analyst', 'admin') && <Button type="primary" icon={<Play size={16} />} onClick={openRun}>运行平衡</Button>}
          </>
        }
      />
      <section className="balance-layout">
        <div className="balance-main">
          <div className="chart-panel">
            <div className="section-heading">
              <div><h2>质量边界瀑布</h2><span>{selected ? selected.tank?.tank_code + ' · ' + dateTime(selected.period_end) : '尚未选择运行'}</span></div>
              {selected && <Tag color={selected.deviation_level === 'investigate' ? 'warning' : 'success'}>{deviationLabels[selected.deviation_level]}</Tag>}
            </div>
            <MassBalanceWaterfall run={selected} />
            {selected && (
              <div className="metric-strip">
                <div><span>BOG / 未解释项</span><strong>{kg(selected.estimated_bog_kg)}</strong></div>
                <div><span>不确定度</span><strong>± {kg(selected.uncertainty_kg)}</strong></div>
                <div><span>偏差率</span><strong>{number.format(selected.deviation_pct)}%</strong></div>
                <div>
                  <span>证据冻结</span>
                  <strong className={selected.evidence_stale ? 'stale-text' : 'frozen-text'}>
                    {selected.evidence_stale ? <><TriangleAlert size={14} /> 证据过期</> : <><Snowflake size={14} /> 冻结一致</>}
                  </strong>
                </div>
              </div>
            )}
            {selected?.evidence_stale && (
              <Alert
                className="freeze-inline-alert"
                type="error"
                showIcon
                icon={<TriangleAlert size={16} />}
                message="该运行的期初/期末快照或期间已确认转移已变化，结果证据过期；提交与复核已锁定，请重新运行平衡生成独立结果，旧结果与审计继续保留。"
              />
            )}
          </div>
          <EvidenceFreezePanel run={selected} />
          <EvidenceBreakdownPanel run={selected} />
        </div>
        <aside className="run-rail">
          <div className="section-heading"><h2>运行历史</h2><span>{store.items.length} 条</span></div>
          <Table<BalanceRun>
            rowKey="id"
            size="small"
            loading={store.loading}
            dataSource={store.items}
            pagination={{ pageSize: 8, showSizeChanger: false }}
            onRow={(item) => ({ onClick: () => store.select(item.id) })}
            rowClassName={(item) => [
              item.id === selected?.id ? 'selected-row' : '',
              item.evidence_stale ? 'stale-row' : ''
            ].filter(Boolean).join(' ')}
            columns={[
              {
                title: '运行', key: 'run',
                render: (_, item) => (
                  <>
                    <strong>#{item.id} · {item.tank?.tank_code ?? item.tank_id}</strong>
                    <div className="secondary">{dateTime(item.period_end)}</div>
                    {item.evidence_stale && (
                      <Tag className="stale-inline-tag" color="error" icon={<TriangleAlert size={11} />}>
                        证据过期 · {(item.stale_changes ?? []).length} 项变化
                      </Tag>
                    )}
                  </>
                )
              },
              {
                title: '状态', dataIndex: 'balance_status', width: 104,
                render: (value: BalanceStatus, item) => (
                  <span className="rail-status-cell">
                    <Tag>{statusLabels[value]}</Tag>
                    <Tooltip title={item.evidence_stale ? '冻结证据与当前期间不一致' : '冻结证据与当前期间一致'}>
                      {item.evidence_stale
                        ? <TriangleAlert size={14} className="stale-icon" aria-label="证据过期" />
                        : <Snowflake size={14} className="frozen-icon" aria-label="冻结一致" />}
                    </Tooltip>
                  </span>
                )
              }
            ]}
          />
          {selected && (
            <div className="workflow-actions">
              {selected.evidence_stale && (selected.balance_status === 'calculating' || selected.balance_status === 'pending_review') && (
                <Alert
                  type="error"
                  showIcon
                  icon={<TriangleAlert size={15} />}
                  message="证据过期，提交与复核已锁定"
                  description={(selected.stale_changes ?? []).map((change) => change.detail).join('；') || '请重新运行平衡生成独立结果。'}
                />
              )}
              {selected.balance_status === 'calculating' && can('process_analyst', 'admin') && (
                <Tooltip title={selected.evidence_stale ? '证据已过期，必须重新计算生成独立结果' : '提交当前冻结证据给独立复核员'}>
                  <Button type="primary" icon={<Send size={16} />} loading={store.working} disabled={selected.evidence_stale} onClick={() => void store.submit(selected)} block>提交独立复核</Button>
                </Tooltip>
              )}
              {selected.balance_status === 'pending_review' && can('reviewer', 'admin') && (
                <>
                  <Tooltip title={selected.evidence_stale ? '证据已过期，不允许接受，请退回分析员重新计算' : '冻结证据一致，可以接受结果'}>
                    <Button type="primary" icon={<CheckCircle2 size={16} />} disabled={selected.evidence_stale} onClick={() => openReview('accepted')} block>接受结果</Button>
                  </Tooltip>
                  <Tooltip title={selected.evidence_stale ? '证据已过期，请由分析员重新计算生成独立结果' : '冻结证据存在疑问时驳回结果'}>
                    <Button danger icon={<XCircle size={16} />} disabled={selected.evidence_stale} onClick={() => openReview('rejected')} block>驳回结果</Button>
                  </Tooltip>
                </>
              )}
              {selected.review_note && <Alert type="info" showIcon message={selected.review_note} />}
            </div>
          )}
        </aside>
      </section>
      <Modal title="运行物理质量平衡" open={runOpen} onCancel={() => setRunOpen(false)} footer={null} destroyOnClose>
        <Form<BalanceRunInput>
          form={runForm}
          layout="vertical"
          onFinish={run}
          requiredMark={false}
          initialValues={{
            period_start: localInputDate(new Date(Date.now() - 24 * 3_600_000)),
            period_end: localInputDate(new Date())
          }}
        >
          <Form.Item name="tank_id" label="储罐" rules={[{ required: true }]}><Select options={tanks.items.map((tank) => ({ value: tank.id, label: tank.tank_code + ' · ' + tank.name }))} /></Form.Item>
          <div className="form-grid">
            <Form.Item name="period_start" label="期间开始" rules={[{ required: true }]}><Input type="datetime-local" /></Form.Item>
            <Form.Item name="period_end" label="期间结束" rules={[{ required: true }]}><Input type="datetime-local" /></Form.Item>
          </div>
          <Alert className="form-alert" type="warning" showIcon message="系统将选择期间边界有效快照并固化当前罐容系数；既有结果不会被覆盖。" />
          <Button type="primary" htmlType="submit" icon={<Play size={16} />} loading={store.working} block>执行计算</Button>
        </Form>
      </Modal>
      <Modal title={reviewTarget === 'accepted' ? '接受平衡结果' : '驳回平衡结果'} open={reviewOpen} onCancel={() => setReviewOpen(false)} footer={null} destroyOnClose>
        <Form form={reviewForm} layout="vertical" onFinish={review} requiredMark={false}>
          <Form.Item name="note" label="独立复核意见" rules={[{ required: true, min: 6, max: 1000 }]}><Input.TextArea rows={4} /></Form.Item>
          <Button type={reviewTarget === 'accepted' ? 'primary' : 'default'} danger={reviewTarget === 'rejected'} htmlType="submit" loading={store.working} block>
            确认{reviewTarget === 'accepted' ? '接受' : '驳回'}
          </Button>
        </Form>
      </Modal>
      {user?.role === 'reviewer' && !store.items.some((item) => item.balance_status === 'pending_review') && (
        <Alert className="bottom-alert" type="info" showIcon message="当前没有待独立复核的平衡运行。" />
      )}
    </>
  )
}
