import { useEffect, useState } from 'react'
import { Check, Copy, FileCode, FileText, Terminal } from '@phosphor-icons/react'
import type { ImportedEntity } from '@/api/client'
import { CodeView } from '@/components/Atl'

// The generated declarations, as the files they would be committed as.
//
// Offered for a repository that wants them; atlantis holds the schema either
// way. Two hosts — the review screen, where the explorer fills the tab, and
// the dialog for an organisation with no database to read, which adds one line
// of framing above it.

// The three commands, in order. `tide plan` prints the change and writes
// nothing; the Callers page prints `tide login` with this organisation's
// address.
const APPLY = 'tide login\ntide plan\ntide apply'

// The namespace is the Postgres schema, which is what the importer produces for
// a table in `public` — so the hand-written first file and an imported one have
// the same shape.
const EXAMPLE_ATL = `entity User in public {
  table "public.users"

  id          varchar(26) primary
  email       varchar(255) not null unique
  created_at  timestamptz default now()
}`

export interface CommitFile {
  path: string
  body: string
}

// filesFor turns declarations into the files they will be committed as: one per
// namespace, because one namespace is one .atl file.
//
// An empty read yields the example, which is what an organisation writing its
// first declaration by hand needs.
export function filesFor(entities: ImportedEntity[], header = ''): CommitFile[] {
  const groups = new Map<string, string[]>()
  for (const e of entities) {
    const list = groups.get(e.namespace)
    if (list) list.push(e.atl)
    else groups.set(e.namespace, [e.atl])
  }
  // The header opens each file once. The server composes it; joining it here
  // is the only thing this knows about it.
  const open = header ? header + '\n' : ''
  const files = [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([ns, atl]) => ({ path: `schema/${ns}.atl`, body: open + atl.join('\n\n') }))

  return files.length > 0 ? files : [{ path: 'schema/public.atl', body: EXAMPLE_ATL }]
}

// The dialog's wrapper: one line of framing, then the explorer.
export function CommitGuide({
  files,
  lede,
}: {
  files: CommitFile[]
  lede?: string
}) {
  return (
    <div className="guide">
      {lede && <p className="guide__lede">{lede}</p>}
      <FileBrowser files={files} />
    </div>
  )
}

// A file list beside the file. The declarations for a database of any size run
// to thousands of lines across a dozen files, and stacking them means scrolling
// past one to reach the next.
export function FileBrowser({ files }: { files: CommitFile[] }) {
  const [open, setOpen] = useState(files[0]?.path ?? '')
  const current = files.find(f => f.path === open) ?? files[0]

  // The list changes when a different namespace is fetched, and a path that is
  // no longer in it would leave the pane on the wrong file.
  useEffect(() => {
    if (!files.some(f => f.path === open)) setOpen(files[0]?.path ?? '')
  }, [files, open])

  if (!current) return null
  const lines = current.body.split('\n')

  return (
    <div className="fbrowse">
      <div className="fbrowse__list" role="tablist" aria-label="Files">
        <div className="fbrowse__listhead">
          {files.length} {files.length === 1 ? 'file' : 'files'}
        </div>
        {files.map(f => (
          <button
            key={f.path}
            type="button"
            role="tab"
            aria-selected={f.path === current.path}
            className={`fbrowse__item ${f.path === current.path ? 'is-active' : ''}`}
            onClick={() => setOpen(f.path)}
          >
            {f.path.endsWith('.atl') ? <FileCode size={13} /> : <FileText size={13} />}
            <span className="fbrowse__name">{basename(f.path)}</span>
          </button>
        ))}
      </div>

      <div className="fbrowse__main">
        <div className="fbrowse__head">
          <span className="mono fbrowse__path">{current.path}</span>
          <span className="fbrowse__lines">
            {lines.length} {lines.length === 1 ? 'line' : 'lines'}
          </span>
          <CopyButton text={current.body} />
        </div>

        <CodeView body={current.body} path={current.path} className="fbrowse__code" />

        {/* Committing the files is half of it; the other half is the three
            commands, so they sit in the frame rather than in a section
            somebody has to scroll to. */}
        <div className="fbrowse__foot">
          <Terminal size={13} />
          <span className="fbrowse__cmd mono">{APPLY.split('\n').join('   ')}</span>
          <CopyButton text={APPLY} />
        </div>
      </div>
    </div>
  )
}

function basename(path: string): string {
  const i = path.lastIndexOf('/')
  return i === -1 ? path : path.slice(i + 1)
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 1600)
    return () => clearTimeout(t)
  }, [copied])

  return (
    <button
      type="button"
      className="btn btn--sm"
      onClick={() => {
        navigator.clipboard.writeText(text).then(
          () => setCopied(true),
          () => setCopied(false),
        )
      }}
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}
