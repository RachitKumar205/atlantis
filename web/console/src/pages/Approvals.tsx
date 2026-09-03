import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle, ShieldChevron, Snowflake, XCircle } from '@phosphor-icons/react'
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

// verdictBadge maps a rehearsal verdict to the badge tone the classes
// already use: green is "the real apply would work", red is "it would fail".
function verdictBadge(verdict: string): string {
  switch (verdict) {
    case 'pass':
      return 'badge--add'
    case 'pass_with_warnings':
      return 'badge--back'
    case 'fail_data':
    case 'fail_structural':
      return 'badge--destroy'
    default:
      return 'badge--plain'
  }
}

export function Approvals() {
  const qc = useQueryClient()
  const { data: me } = useMe()
  const [openID, setOpenID] = useState<string | null>(null)
  const [confirm, setConfirm] = useState<{ plan: SchemaPlanSummary; approve: boolean } | null>(null)
  const [overrideFor, setOverrideFor] = useState<SchemaPlanSummary | null>(null)
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
  const { data: freezes } = useQuery({
    queryKey: ['freeze-windows'],
    queryFn: api.freezes.list,
  })
  const activeFreeze = (freezes?.windows ?? []).find(w =>
    new Date(w.starts_at) <= new Date() && new Date() < new Date(w.ends_at))
  const { data: detail } = useQuery({
    queryKey: ['schema-plan', openID],
    queryFn: () => api.plans.get(openID as string),
    enabled: openID !== null,
  })

  // Approve elevates first; reject does not. The worst a wrongly-rejected plan
  // costs is a re-plan, and a trip to Cloud in front of "no" is how a review
  // stops being said.
  const approve = useMutation({
    mutationFn: async ({ id, assertion }: { id: string; assertion: string }) => {
      await api.auth.sudo(assertion)
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

  // Override is a separate mutation and a separate route: it is the
  // admin-only path past a freeze, a protection, or the self-approval
  // refusal, and the org server demands the reason.
  const override = useMutation({
    mutationFn: async ({ id, assertion }: { id: string; assertion: string }) => {
      await api.auth.sudo(assertion)
      return api.plans.override(id, reason)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['schema-plans'] })
      fire('Overridden. The caller can apply it now, freeze or no freeze.')
      setOverrideFor(null)
      setOpenID(null)
      setReason('')
    },
  })

  // Rehearsal is slow by design — the server clones the managed database —
  // so the button shows a spinner and the verdict lands on the row.
  const rehearse = useMutation({
    mutationFn: (id: string) => api.plans.rehearse(id),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['schema-plans'] })
      fire(`Rehearsed: ${res.verdict.replaceAll('_', ' ')}${res.error ? ` — ${res.error}` : ''}`)
    },
    onError: (e: Error) => fire(e.message),
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
        {activeFreeze && (
          <div className="notice" style={{ marginBottom: 14, display: 'flex', gap: 8, alignItems: 'center' }}>
            <Snowflake size={16} />
            <span>
              A freeze holds every apply until{' '}
              <strong>{new Date(activeFreeze.ends_at).toLocaleString()}</strong>
              {activeFreeze.reason ? <> — {activeFreeze.reason}</> : null}. Approvals stay
              open; approved changes queue behind the window, and an override passes it.
            </span>
          </div>
        )}

        {isLoading && <p className="muted">Loading…</p>}

        {!isLoading && plans.length === 0 && (
          <div className="empty">
            <ShieldChevron size={20} />
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
                    {p.rehearsal_verdict && (
                      <span className={`badge ${verdictBadge(p.rehearsal_verdict)}`}>
                        rehearsed · {p.rehearsal_verdict.replaceAll('_', ' ')}
                      </span>
                    )}
                    <span className="muted">
                      requested by {p.requested_by}
                      {p.requested_by_actor
                        ? <> for <span className="mono">{p.requested_by_actor}</span></>
                        : ' (unattributed)'}
                    </span>
                  </div>
                  <div className="muted" style={{ marginTop: 4, fontSize: 12 }}>
                    {p.expired
                      ? 'Expired — the caller must re-plan and ask again.'
                      : <>Needs <strong>{p.approver_role}</strong>{p.expires_at ? ` · expires ${p.expires_at}` : ''}</>}
                  </div>
                </div>
                <div className="row" style={{ gap: 8 }}>
                  {me?.role !== 'viewer' && (
                    <button
                      className="btn btn--sm btn--ghost"
                      disabled={rehearse.isPending}
                      title="Run this plan's SQL against a clone of the real data"
                      onClick={() => rehearse.mutate(p.plan_id)}
                    >
                      {rehearse.isPending && <span className="spin" />}
                      Rehearse
                    </button>
                  )}
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
                      <CheckCircle size={12} /><span>Approve</span>
                    </button>
                    <button
                      className="btn btn--sm btn--danger"
                      disabled={!mine || p.expired || reason.trim() === '' || reject.isPending}
                      title={reason.trim() === '' ? 'A rejection needs a reason' : undefined}
                      onClick={() => reject.mutate(p.plan_id)}
                    >
                      <XCircle size={12} /><span>Reject</span>
                    </button>
                    {me?.role === 'admin' && (
                      <button
                        className="btn btn--sm btn--ghost"
                        disabled={p.expired || reason.trim() === '' || override.isPending}
                        title={reason.trim() === ''
                          ? 'An override needs a reason'
                          : 'Approve past a freeze, a protection, or the self-approval refusal'}
                        onClick={() => setOverrideFor(p)}
                      >
                        Override
                      </button>
                    )}
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
          icon={<CheckCircle />}
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
          onConfirm={(assertion) => approve.mutate({ id: confirm.plan.plan_id, assertion })}
        />
      )}

      {overrideFor && (
        <SudoConfirmDialog
          title="Override this plan"
          icon={<Snowflake />}
          body={
            <p>
              <strong>{overrideFor.caller}</strong>'s{' '}
              <strong>{planClassLabel(overrideFor.change_class)}</strong> change is approved past
              whatever held it — a freeze window, a protected entity, or the requester being the
              approver. The reason is recorded in both audit ledgers.
            </p>
          }
          confirmLabel="Override"
          pending={override.isPending}
          error={override.error ? (override.error as Error).message : null}
          onCancel={() => { override.reset(); setOverrideFor(null) }}
          onConfirm={(assertion) => override.mutate({ id: overrideFor.plan_id, assertion })}
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
