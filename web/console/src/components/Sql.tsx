import { Fragment, type ReactNode } from 'react'

// PostgreSQL keyword + type sets. Case-insensitive lookup. Compiled once
// at module load. Kept narrow on purpose — the SQL we render is server-
// generated migration/plan output, so we only need the lexemes that
// actually show up (DDL + a handful of DML). Extend as needed.
const KEYWORDS = new Set([
  'select', 'from', 'where', 'and', 'or', 'not', 'in', 'is', 'null', 'true', 'false',
  'create', 'table', 'alter', 'add', 'drop', 'column', 'index', 'constraint',
  'primary', 'key', 'foreign', 'references', 'unique', 'default', 'check',
  'insert', 'into', 'values', 'update', 'set', 'delete', 'returning',
  'join', 'left', 'right', 'inner', 'outer', 'full', 'cross', 'on', 'as', 'using',
  'if', 'exists', 'cascade', 'restrict', 'schema', 'view',
  'with', 'order', 'by', 'group', 'having', 'limit', 'offset', 'distinct',
  'case', 'when', 'then', 'else', 'end',
  'union', 'intersect', 'except', 'all',
  'begin', 'commit', 'rollback', 'transaction',
  'grant', 'revoke', 'to',
  'between', 'like', 'ilike',
  'partition', 'of', 'range', 'list', 'hash',
  'asc', 'desc', 'nulls', 'first', 'last',
  'returning',
])

const TYPES = new Set([
  'text', 'varchar', 'char', 'character',
  'int', 'integer', 'int2', 'int4', 'int8',
  'bigint', 'smallint',
  'numeric', 'decimal', 'real', 'double', 'precision', 'float', 'float4', 'float8',
  'boolean', 'bool',
  'timestamp', 'timestamptz', 'date', 'time', 'timetz', 'interval',
  'json', 'jsonb', 'bytea', 'uuid',
  'array', 'bigserial', 'serial', 'smallserial',
])

// highlightSQL tokenizes a single SQL string into React nodes with
// `.sql-*` className spans. The unhandled-character path coalesces into
// a buffer so we don't emit one node per whitespace.
function highlightSQL(sql: string): ReactNode[] {
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
  const n = sql.length
  while (i < n) {
    const ch = sql[i]

    // Block comment /* … */
    if (ch === '/' && sql[i + 1] === '*') {
      const end = sql.indexOf('*/', i + 2)
      const close = end === -1 ? n : end + 2
      push('sql-cmt', sql.slice(i, close))
      i = close
      continue
    }

    // Line comment -- …
    if (ch === '-' && sql[i + 1] === '-') {
      const nl = sql.indexOf('\n', i)
      const close = nl === -1 ? n : nl
      push('sql-cmt', sql.slice(i, close))
      i = close
      continue
    }

    // String literal 'foo' with '' escape
    if (ch === "'") {
      let j = i + 1
      while (j < n) {
        if (sql[j] === "'" && sql[j + 1] === "'") {
          j += 2
          continue
        }
        if (sql[j] === "'") {
          j++
          break
        }
        j++
      }
      push('sql-str', sql.slice(i, j))
      i = j
      continue
    }

    // Quoted identifier "foo"
    if (ch === '"') {
      let j = i + 1
      while (j < n && sql[j] !== '"') j++
      if (j < n && sql[j] === '"') j++
      push('sql-ident', sql.slice(i, j))
      i = j
      continue
    }

    // Numeric literal — start with digit OR with a dot followed by digit
    if (
      (ch >= '0' && ch <= '9') ||
      (ch === '.' && sql[i + 1] >= '0' && sql[i + 1] <= '9')
    ) {
      let j = i
      while (j < n) {
        const c = sql[j]
        if ((c >= '0' && c <= '9') || c === '.' || c === 'e' || c === 'E') {
          j++
        } else if ((c === '+' || c === '-') && (sql[j - 1] === 'e' || sql[j - 1] === 'E')) {
          j++
        } else break
      }
      push('sql-num', sql.slice(i, j))
      i = j
      continue
    }

    // Identifier (possibly a keyword or type)
    if ((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch === '_') {
      let j = i
      while (j < n) {
        const c = sql[j]
        if (
          (c >= 'a' && c <= 'z') ||
          (c >= 'A' && c <= 'Z') ||
          (c >= '0' && c <= '9') ||
          c === '_'
        ) {
          j++
        } else break
      }
      const word = sql.slice(i, j)
      const lower = word.toLowerCase()
      if (KEYWORDS.has(lower)) push('sql-kw', word)
      else if (TYPES.has(lower)) push('sql-type', word)
      else {
        buf += word
      }
      i = j
      continue
    }

    // Everything else — whitespace, punctuation, operators — into buffer.
    buf += ch
    i++
  }
  flush()
  return out
}

interface SqlProps {
  children: string
  className?: string
  style?: React.CSSProperties
}

// Sql renders a syntax-highlighted SQL block. Drop-in replacement for
// `<pre>{sqlString}</pre>` — caller controls the wrapping container.
export function Sql({ children, className = '', style }: SqlProps) {
  return (
    <pre className={`sql ${className}`} style={style}>
      <code>
        {highlightSQL(children).map((node, i) => (
          <Fragment key={i}>{node}</Fragment>
        ))}
      </code>
    </pre>
  )
}
