import { describe, expect, it } from 'vitest'
import { physicalTable, snakeCase } from './physical'

// The four cases are schema_test.go's TestSnakeCase verbatim. They are here so
// that a change to the Go convention shows up as a failure on this side rather
// than as a table name the console reports and Postgres does not have.
describe('snakeCase matches internal/schema', () => {
  it.each([
    ['Account', 'account'],
    ['ProductVariant', 'product_variant'],
    ['APIKey', 'api_key'],
    ['CartItem', 'cart_item'],
  ])('%s -> %s', (input, want) => {
    expect(snakeCase(input)).toBe(want)
  })
})

describe('physicalTable', () => {
  it('flattens namespace and name when the entity declares no table', () => {
    expect(physicalTable('catalog', 'ProductVariant')).toBe('atlantis.catalog_product_variant')
  })

  it('takes a qualified override as written', () => {
    expect(physicalTable('rnacen', 'AuthPermission', 'rnacen.auth_permission'))
      .toBe('rnacen.auth_permission')
  })

  // EntitySchema puts a bare override in public, not in the entity's namespace.
  it('puts an unqualified override in public', () => {
    expect(physicalTable('catalog', 'Product', 'products')).toBe('public.products')
  })
})
