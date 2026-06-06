// Generic rows table for the sandbox surface — SQL results, Inspect.Sample,
// Inspect.Find all return `Array<Record<string, unknown>>`. We render a
// dense, mono-font table; columns are discovered from the first row's
// keys, sorted for determinism. Null cells show as a faded "NULL"
// glyph. Bytes (b64:-prefixed strings) render as a length-only badge so
// JSONB blobs don't blow up the layout.

interface Props {
  rows: Array<Record<string, unknown>>
  empty?: string
}

export function RowsTable({ rows, empty = '(no rows)' }: Props) {
  if (rows.length === 0) {
    return <div className="rows-empty">{empty}</div>
  }
  // Discover columns from the first row; assume schema-uniform shape
  // (which the sandbox always produces because the executor projects
  // a fixed column list per row).
  const cols = Object.keys(rows[0]).sort()
  return (
    <div className="rows-wrap">
      <table className="rows-table mono">
        <thead>
          <tr>
            {cols.map(c => <th key={c}>{c}</th>)}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {cols.map(c => <td key={c}>{renderCell(r[c])}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function renderCell(v: unknown): React.ReactNode {
  if (v === null || v === undefined) return <span className="cell-null">NULL</span>
  if (typeof v === 'string') {
    if (v.startsWith('b64:')) {
      const len = Math.max(0, ((v.length - 4) * 3) >> 2)
      return <span className="cell-bytes">{`<${len} bytes>`}</span>
    }
    return v
  }
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (Array.isArray(v)) {
    if (v.length > 8) {
      return <span className="cell-vec">{`[${v[0]}, … ${v.length} values]`}</span>
    }
    return JSON.stringify(v)
  }
  return JSON.stringify(v)
}
