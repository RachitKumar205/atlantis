// The physical table an entity maps to.
//
// Ported from internal/schema/schema.go — EntitySchema, EntityPhysicalTable
// and SnakeCase. physical.test.ts carries that package's own cases, so a
// change to the convention on either side fails here.

// snakeCase mirrors schema.SnakeCase: an underscore goes in ahead of a capital
// that follows a lowercase letter, or that starts a lowercase run after other
// capitals. APIKey is api_key, not a_p_i_key.
export function snakeCase(s: string): string {
  let out = ''
  for (let i = 0; i < s.length; i++) {
    const r = s[i]
    if (i > 0 && r >= 'A' && r <= 'Z') {
      const prev = s[i - 1]
      const next = i + 1 < s.length ? s[i + 1] : ''
      if (
        (prev >= 'a' && prev <= 'z') ||
        (next >= 'a' && next <= 'z' && prev >= 'A' && prev <= 'Z')
      ) {
        out += '_'
      }
    }
    out += r >= 'A' && r <= 'Z' ? r.toLowerCase() : r
  }
  return out
}

// physicalTable returns the schema-qualified table, unquoted.
//
// A `table "schema.table"` override is taken as written; a bare `table "name"`
// lives in public; an entity with no override lives in atlantis under the
// flattened `<namespace>_<snake>`.
export function physicalTable(namespace: string, name: string, tableName?: string): string {
  if (tableName) {
    return tableName.includes('.') ? tableName : `public.${tableName}`
  }
  return `atlantis.${namespace}_${snakeCase(name)}`
}
