import { useEffect, useState, type FormEvent } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { ArrowLeft, ArrowSquareIn, PencilSimple, X } from '@phosphor-icons/react'
import { ApiError, TLS_REQUIRED, api, type SchemaImportStarted } from '@/api/client'
import { CommitGuide, filesFor } from '@/components/CommitGuide'
import {
  Alert,
  AlertDescription,
  AlertDialog,
  AlertTitle,
  Progress,
  ProgressLabel,
  ProgressValue,
  RadioCardGroup,
} from '@/components/ui'

// Getting an organisation from an empty console to its first `tide apply`.
//
// Four steps, of which the middle two are skipped by an organisation with no
// database to read: what atlantis wants, where to read it from, what came back,
// and the files to commit. The last step is the one that matters — declarations
// nobody can find in their repository are the same as no declarations.
//
// Nothing here is applied. The read runs in a read-only transaction and returns
// text; `tide plan` still decides what reaches a database.

type Step = 'welcome' | 'connect' | 'commit'

// The steps Progress counts. `welcome` is a choice rather than work, so it is
// not one of them and shows no bar.
//
// Review is not here. Reading a database returns 209 tables and 156
// suggestions, which is a screen's worth of work rather than a dialog's, so a
// successful import closes this and navigates to the import's own URL.
const STEPS: { step: Step; name: string }[] = [
  { step: 'connect', name: 'Connect' },
  { step: 'commit', name: 'Commit' },
]

const HEADING: Record<Step, { title: string; sub: string }> = {
  welcome: {
    title: 'Set up your schema',
    sub: 'Atlantis manages the schema your repository declares.',
  },
  connect: {
    title: 'Import a database',
    sub: 'Read an existing Postgres and generate the declarations for it.',
  },
  commit: {
    title: 'Your first declaration',
    sub: 'Write one, and atlantis holds it from the first apply.',
  },
}

export function OnboardingDialog({
  start = 'welcome',
  onClose,
}: {
  // The button on the schema page opens at `connect`: an organisation that has
  // already been through this does not need the first screen again.
  start?: Step
  onClose: () => void
}) {
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>(start)
  const [path, setPath] = useState('import')
  const [dsn, setDsn] = useState('')

  // Never remembered and never defaulted on. The server decides when it is
  // needed and says so with TLS_REQUIRED; until then every request is sent
  // requiring TLS, so the same switch cannot reach a production database
  // without somebody answering for it.
  const [askInsecure, setAskInsecure] = useState(false)

  const run = useMutation<SchemaImportStarted, Error, boolean>({
    mutationFn: (allowInsecure: boolean) => api.schemaImport.run(dsn.trim(), allowInsecure),
    // The dialog is done the moment the read succeeds. It closes rather than
    // growing a third step, and the review opens at the import's own URL, which
    // survives a reload and can be linked to.
    onSuccess: started => {
      setAskInsecure(false)
      onClose()
      navigate({ to: '/imports/$id', params: { id: started.import_id } })
    },
    onError: err => {
      const tls = err instanceof ApiError && err.code === TLS_REQUIRED
      // Cleared as well as set. The question is only asked while it is the
      // whole of the answer, and a later attempt failing for its own reason
      // was hidden behind a question already answered — the dialog went idle
      // showing nothing at all.
      setAskInsecure(tls)
    },
  })

  // The refusal that opened the question is not also printed behind it: the
  // dialog is the whole of the answer while it is up.
  const error = run.isError && !askInsecure ? run.error.message : null

  // Escape and the scrim close the dialog as well as the button does. All
  // three call onClose, so the caller records one dismissal for any of them.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  function submit(e: FormEvent) {
    e.preventDefault()
    run.mutate(false)
  }

  return (
    <div
      className="overlay is-open"
      onMouseDown={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        className="modal modal--onboard"
        role="dialog"
        aria-modal
        aria-label="Set up your schema"
      >
        <Head step={step} onClose={onClose} />

        {step === 'welcome' && (
          <Welcome
            path={path}
            setPath={setPath}
            onNext={() => setStep(path === 'import' ? 'connect' : 'commit')}
            onClose={onClose}
          />
        )}

        {step === 'connect' && (
          <Connect
            dsn={dsn}
            setDsn={setDsn}
            busy={run.isPending}
            error={error}
            onSubmit={submit}
            onBack={start === 'welcome' ? () => setStep('welcome') : null}
          />
        )}

        {step === 'commit' && (
          <>
            <div className="modal__body onboard__body">
              <CommitGuide
                files={filesFor([])}
                lede="This is the shape of a declaration. Write it, run tide apply, and atlantis holds it from then on."
              />
            </div>
            <div className="modal__foot">
              <button className="btn btn--brass" type="button" onClick={onClose}>
                Done
              </button>
            </div>
          </>
        )}
      </div>

      {askInsecure && (
        <AlertDialog
          title="This database has no TLS"
          confirmLabel="Read it anyway"
          cancelLabel="Cancel"
          tone="danger"
          busy={run.isPending}
          onConfirm={() => run.mutate(true)}
          onCancel={() => {
            setAskInsecure(false)
            run.reset()
          }}
        >
          Reading it sends the password across the internet in clear, where anything
          between here and the server can read it. Public read-only datasets are the
          case this exists for. Never continue for a database whose credential is
          worth stealing.
        </AlertDialog>
      )}
    </div>
  )
}

// The bar counts Connect and Commit. `commit` is reached only by the path that
// skipped Connect, so it draws no bar: a step marked done for work that never
// ran says the database was read.
function Head({ step, onClose }: { step: Step; onClose: () => void }) {
  const at = step === 'connect' ? 0 : -1
  const { title, sub } = HEADING[step]

  return (
    <div className="modal__head">
      <div className="modal__headtext">
        <span className="modal__title">{title}</span>
        <div className="modal__sub">{sub}</div>
        {at >= 0 && (
          <Progress value={at + 1} max={STEPS.length}>
            <ProgressLabel>{STEPS[at].name}</ProgressLabel>
            <ProgressValue>
              Step {at + 1} of {STEPS.length}
            </ProgressValue>
          </Progress>
        )}
      </div>
      <button type="button" className="btn btn--ghost btn--icon" aria-label="Close" onClick={onClose}>
        <X size={14} />
      </button>
    </div>
  )
}

// ── Step 1: what this is, and the two ways in ──────────────────────────────
function Welcome({
  path,
  setPath,
  onNext,
  onClose,
}: {
  path: string
  setPath: (v: string) => void
  onNext: () => void
  onClose: () => void
}) {
  return (
    <>
      <div className="modal__body onboard__body">
        <p className="onboard__lede">
          Your schema lives in your repository as <span className="mono">.atl</span> files.{' '}
          <span className="mono">tide plan</span> shows what would change,{' '}
          <span className="mono">tide apply</span> makes it. Nothing changes a database
          until you run it.
        </p>

        <RadioCardGroup
          name="onboarding-path"
          value={path}
          onChange={setPath}
          options={[
            {
              value: 'import',
              title: 'Import an existing database',
              description:
                'Read a Postgres you already run. Every schema in it becomes a namespace, and every table a declaration you can commit.',
              icon: <ArrowSquareIn size={17} />,
            },
            {
              value: 'byhand',
              title: 'Start without one',
              description:
                'Write the first declaration by hand. The next step shows where it goes and what to run.',
              icon: <PencilSimple size={17} />,
            },
          ]}
        />
      </div>
      <div className="modal__foot">
        <button className="btn btn--ghost" type="button" onClick={onClose}>
          Not now
        </button>
        <span className="spacer" style={{ flex: 1 }} />
        <button className="btn btn--brass" type="button" onClick={onNext}>
          Continue
        </button>
      </div>
    </>
  )
}

// ── Step 2: the connection ─────────────────────────────────────────────────
function Connect({
  dsn,
  setDsn,
  busy,
  error,
  onSubmit,
  onBack,
}: {
  dsn: string
  setDsn: (v: string) => void
  busy: boolean
  error: string | null
  onSubmit: (e: FormEvent) => void
  onBack: (() => void) | null
}) {
  return (
    <form onSubmit={onSubmit}>
      <div className="modal__body onboard__body">
        <p className="onboard__lede">
          Every schema is read, and each becomes a namespace of the same name. Nothing is
          written to your database now: the read runs in a read-only transaction. This
          becomes the database atlantis manages, so the connection string is kept —
          sealed, on your organisation's own server. This console records only the host.
        </p>

        <label className="field">
          <span className="field__label">Connection string</span>
          <input
            className="input mono"
            type="password"
            autoComplete="off"
            placeholder="postgres://readonly@db.example.com:5432/app?sslmode=require"
            value={dsn}
            onChange={e => setDsn(e.target.value)}
            required
            autoFocus
          />
          <span className="field__hint">Must be reachable from the internet.</span>
        </label>

        {error && (
          <Alert variant="error">
            <AlertTitle>The database could not be read</AlertTitle>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
      </div>
      <div className="modal__foot">
        {onBack && (
          <button className="btn btn--ghost" type="button" onClick={onBack}>
            <ArrowLeft size={13} /> Back
          </button>
        )}
        <span className="spacer" style={{ flex: 1 }} />
        <button className="btn btn--brass" type="submit" disabled={busy}>
          {busy && <span className="spin" />}
          {busy ? 'Reading the schema…' : 'Read the schema'}
        </button>
      </div>
    </form>
  )
}

