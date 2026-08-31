import { type ReactNode } from 'react'

// Highlighting for the .atl declarations the importer generates.
//
// The keyword set is internal/dsl/token.go's `keywords` map, verbatim. A word
// this does not know renders as an identifier, which is what a field name is,
// so a keyword added to the language and not to this list reads as a field
// rather than as an error.
//
// One line at a time, because the file browser draws one element per line for
// its gutter. Nothing in the grammar spans a line: a string is single-line and
// a comment runs to the end of one.

const KEYWORDS = new Set([
  'entity', 'hypertable', 'in', 'primary', 'identity', 'serial', 'not', 'null',
  'unique', 'default', 'check', 'backfill', 'references', 'has_many', 'has_one',
  'via', 'index', 'by', 'on', 'hnsw', 'ops', 'cosine', 'l2', 'ip', 'gin',
  'partial', 'where', 'is', 'asc', 'desc', 'cache', 'read_through', 'ttl',
  'tag', 'invalidate_on', 'write', 'self', 'consistency', 'strict', 'eventual',
  'query_timeout', 'soft_delete', 'touch_on_update', 'partition', 'table',
  'expr', 'as', 'deferrable', 'true', 'false', 'now', 'raw', 'delete', 'update',
  'cascade', 'restrict', 'set', 'query', 'procedure', 'for', 'input', 'output',
  'steps', 'sql', 'touches', 'invalidate', 'insert', 'job', 'args', 'schedule',
  'queue', 'retries', 'timeout', 'heartbeat', 'enqueue', 'visible_to',
  'ttl_field', 'chunk_time_interval', 'workflow', 'step', 'compensate',
  'state', 'ephemeral',
])

// Column types. Separate from the keywords so a type reads differently from
// the `not null` beside it — which is the distinction being scanned for when
// reading a generated declaration.
const TYPES = new Set([
  'text', 'varchar', 'char', 'int', 'integer', 'bigint', 'smallint',
  'numeric', 'decimal', 'real', 'double', 'float',
  'boolean', 'bool', 'timestamptz', 'timestamp', 'date', 'time', 'interval',
  'json', 'jsonb', 'bytea', 'uuid', 'vector', 'bigserial', 'smallserial',
])

// highlightAtl tokenises one line into spans.
export function highlightAtl(line: string): ReactNode[] {
  const out: ReactNode[] = []
  let buf = ''
  let key = 0

  const flush = () => {
    if (buf) {
      out.push(buf)
      buf = ''
    }
  }
  const push = (cls: string, text: string) => {
    flush()
    out.push(
      <span key={key++} className={cls}>
        {text}
      </span>,
    )
  }

  let i = 0
  const n = line.length
  // Set by `entity`, `in` and `references`, so the name that follows takes the
  // colour of the thing it names rather than of a field.
  let expect: 'entity' | 'namespace' | 'ref' | null = null

  while (i < n) {
    const c = line[i]

    // Comment to end of line.
    if (c === '/' && line[i + 1] === '/') {
      push('tok-comment', line.slice(i))
      break
    }

    if (c === '"') {
      let j = i + 1
      while (j < n && line[j] !== '"') {
        if (line[j] === '\\') j++
        j++
      }
      push('tok-string', line.slice(i, Math.min(j + 1, n)))
      i = Math.min(j + 1, n)
      continue
    }

    if (c >= '0' && c <= '9') {
      let j = i
      while (j < n && /[0-9._]/.test(line[j])) j++
      push('tok-number', line.slice(i, j))
      i = j
      continue
    }

    if (/[A-Za-z_]/.test(c)) {
      let j = i
      while (j < n && /[A-Za-z0-9_.]/.test(line[j])) j++
      const word = line.slice(i, j)
      const lower = word.toLowerCase()

      if (expect) {
        push(expect === 'entity' ? 'tok-entity' : 'tok-ref', word)
        expect = null
      } else if (KEYWORDS.has(lower)) {
        push('tok-keyword', word)
        if (lower === 'entity' || lower === 'hypertable') expect = 'entity'
        else if (lower === 'in' || lower === 'references') expect = 'ref'
      } else if (TYPES.has(lower)) {
        push('tok-type', word)
      } else {
        push('tok-ident', word)
      }
      i = j
      continue
    }

    if ('{}()[],;'.includes(c)) {
      push('tok-punct', c)
      i++
      continue
    }

    buf += c
    i++
  }

  flush()
  return out
}

// highlightYaml covers tide.yaml: a key, its value, and comments. The file is
// four lines and this is the whole of its grammar.
export function highlightYaml(line: string): ReactNode[] {
  const hash = line.indexOf('#')
  if (hash === 0) return [<span key="c" className="tok-comment">{line}</span>]

  const m = /^(\s*-?\s*)([A-Za-z_][A-Za-z0-9_]*)(:)(.*)$/.exec(line)
  if (m) {
    return [
      m[1],
      <span key="k" className="tok-keyword">{m[2]}</span>,
      <span key="p" className="tok-punct">{m[3]}</span>,
      <span key="v" className="tok-string">{m[4]}</span>,
    ]
  }
  // A bare list item: `  - schema`
  const li = /^(\s*)(-)(\s+)(.*)$/.exec(line)
  if (li) {
    return [
      li[1],
      <span key="d" className="tok-punct">{li[2]}</span>,
      li[3],
      <span key="v" className="tok-string">{li[4]}</span>,
    ]
  }
  return [line]
}

// highlightFor picks the grammar from the file name. A path this does not
// recognise renders unhighlighted rather than wrongly.
export function highlightFor(path: string): (line: string) => ReactNode[] {
  if (path.endsWith('.atl')) return highlightAtl
  if (path.endsWith('.yaml') || path.endsWith('.yml')) return highlightYaml
  return (line: string) => [line]
}

// CodeView renders a file as numbered, highlighted lines.
//
// One element per line, numbered by a CSS counter so the gutter is not
// selected with the code beside it: a declaration copied by hand with line
// numbers in it will not parse.
//
// Shared by the file explorer and the declaration disclosures, so a change to
// how code is drawn reaches both.
export function CodeView({
  body,
  path,
  className = '',
}: {
  body: string
  // Decides the grammar. A declaration shown on its own passes a bare
  // extension.
  path: string
  className?: string
}) {
  const paint = highlightFor(path)
  const lines = body.split('\n')

  return (
    <div className={`codeview ${className}`}>
      {/* An empty line renders empty. A space placeholder holds the line
          height but is selected with the rest, so a blank line between
          declarations pastes as whitespace; CSS gives the height instead. */}
      {lines.map((l, i) => (
        <div className="codeview__line" key={i}>
          {paint(l)}
        </div>
      ))}
    </div>
  )
}
