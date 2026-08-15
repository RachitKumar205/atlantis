import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, ShieldQuestion, XCircle } from 'lucide-react'
import { api, planClassBadge, planClassLabel, type SchemaPlanSummary } from '@/api/client'
import { useMe } from '@/hooks/useAuth'
import { PageShell } from '@/components/PageShell'
import { SudoConfirmDialog } from '@/pages/Settings'

// Changes waiting on a human.
//
// The ordering on this page is the whole argument for it existing. A reviewer
// is shown the proposed .atl source FIRST and the emitted SQL second, because
// they are approving schema — the SQL is what the emitter made of it. Asked to
// sign off on `ALTER TABLE ... DROP COLUMN` with no sight of the declaration
// that produced it, a reviewer can only check that the DDL is well-formed,
// which is not review.

// The wire carries the proto enum name; every other surface names classes
// without the prefix. planClassLabel does the stripping, and lives in client.ts
// beside planClassBadge so the Schema page's pending strip spells a class the
// same way this page does.

// Badge modifiers come from planClassBadge, not from a second map here. A
// local one would drift from the History and Operations pages the first time
// a class was added, and the badge for a destructive change is exactly the
// signal that must not quietly fall back to grey.

export function Approvals() {
  const qc = useQueryClient()
  const { data: me } = useMe()
  const [openID, setOpenID] = useState<string | null>(null)
  const [confirm, setConfirm] = useState<{ plan: SchemaPlanSummary; approve: boolean } | null>(null)
  const [reason, setReason] = useState('')
  const [toast, setToast] = useState<string | null>(null)

  const fire = (msg: string) => {
    setToast(msg)
    setTimeout(() => setToast(null), 4000)
  }

  const { data, isLoading } = useQuery({
    queryKey: ['schema-plans', 'pending_approval'],
    queryFn: () => api.plans.list('pending_approval'),
  })
  const { data: detail } = useQuery({
    queryKey: ['schema-plan', openID],
    queryFn: () => api.plans.get(openID as string),
    enabled: openID !== null,
  })

  // Approve elevates first; reject does not. The worst a wrongly-rejected plan
  // costs is a re-plan, and putting a password prompt in front of "no" is how
  // reviewers stop saying it.
  const approve = useMutation({
    mutationFn: async ({ id, password }: { id: string; password: string }) => {
      await api.auth.sudo(password)
      return api.plans.approve(id, reason)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['schema-plans'] })
      fire('Approved. The caller can apply it now.')
      setConfirm(null)
      setOpenID(null)
      setReason('')
    },
  })

  const reject = useMutation({
    mutationFn: (id: string) => api.plans.reject(id, reason),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['schema-plans'] })
      fire('Rejected. The reason is what the caller will be told.')
      setConfirm(null)
      setOpenID(null)
      setReason('')
    },
    onError: (e: Error) => fire(e.message),
  })

  const plans = data?.plans ?? []

  return (
    <PageShell title="Approvals" sub="schema changes waiting on a decision">
      <div className="page__bodyinner">
        {isLoading && <p className="muted">Loading…</p>}

        {!isLoading && plans.length === 0 && (
          <div className="empty">
            <ShieldQuestion size={20} />
            <p>Nothing is waiting. Changes appear here when the change policy asks for a human.</p>
          </div>
        )}

        {plans.map(p => {
          const mine = me?.role === p.approver_role
          const open = openID === p.plan_id
          return (
            <div className="card" key={p.plan_id} style={{ marginBottom: 12 }}>
              <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center' }}>
                <div>
                  <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                    <span className={`badge badge--${planClassBadge(planClassLabel(p.change_class))}`}>
                      {planClassLabel(p.change_class)}
                    </span>
                    <strong>{p.caller}</strong>
                    <span className="muted">requested by {p.requested_by}</span>
                  </div>
                  <div className="muted" style={{ marginTop: 4, fontSize: 12 }}>
                    {p.expired
                      ? 'Expired — the caller must re-plan and ask again.'
                      : <>Needs <strong>{p.approver_role}</strong>{p.expires_at ? ` · expires ${p.expires_at}` : ''}</>}
                  </div>
                </div>
                <div className="row" style={{ gap: 8 }}>
                  <button className="btn btn--sm" onClick={() => setOpenID(open ? null : p.plan_id)}>
                    {open ? 'Hide' : 'Review'}
                  </button>
                </div>
              </div>

              {open && (
                <div style={{ marginTop: 14 }}>
                  {/* Source first. See the note at the top of this file. */}
                  <h4>Proposed schema</h4>
                  {detail?.plan.files.map(f => (
                    <div key={f.path} style={{ marginBottom: 10 }}>
                      <div className="muted" style={{ fontSize: 12, marginBottom: 4 }}>{f.path}</div>
                      <pre style={{ overflowX: 'auto', fontSize: 11, margin: 0 }}>{f.content}</pre>
                    </div>
                  ))}

                  <h4>SQL this will run</h4>
                  <pre style={{ overflowX: 'auto', fontSize: 11 }}>{detail?.plan.up_sql || '(none)'}</pre>

                  <label className="lbl" htmlFor={`reason-${p.plan_id}`}>Reason</label>
                  <input
                    id={`reason-${p.plan_id}`}
                    className="input"
                    value={reason}
                    placeholder="Required to reject; recorded either way"
                    onChange={e => setReason(e.target.value)}
                  />

                  <div className="row" style={{ gap: 8, marginTop: 12 }}>
                    <button
                      className="btn btn--sm"
                      disabled={!mine || p.expired || approve.isPending}
                      title={mine ? undefined : `Only ${p.approver_role} can decide this`}
                      onClick={() => setConfirm({ plan: p, approve: true })}
                    >
                      <CheckCircle2 size={12} /><span>Approve</span>
                    </button>
                    <button
                      className="btn btn--sm btn--danger"
                      disabled={!mine || p.expired || reason.trim() === '' || reject.isPending}
                      title={reason.trim() === '' ? 'A rejection needs a reason' : undefined}
                      onClick={() => reject.mutate(p.plan_id)}
                    >
                      <XCircle size={12} /><span>Reject</span>
                    </button>
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </div>

      {confirm?.approve && (
        <SudoConfirmDialog
          title="Approve this change"
          icon={<CheckCircle2 />}
          body={
            <p>
              <strong>{confirm.plan.caller}</strong> will be able to apply a{' '}
              <strong>{planClassLabel(confirm.plan.change_class)}</strong> change. Approval is
              recorded against your account and against this exact schema — if the caller
              submits anything else, it will not carry over.
            </p>
          }
          confirmLabel="Approve"
          pending={approve.isPending}
          error={approve.error ? (approve.error as Error).message : null}
          onCancel={() => { approve.reset(); setConfirm(null) }}
          onConfirm={(password) => approve.mutate({ id: confirm.plan.plan_id, password })}
        />
      )}

      {toast && (
        <div className="toast-wrap">
          <div className="toast"><span className="dot" /><span>{toast}</span></div>
        </div>
      )}
    </PageShell>
  )
}
