import assert from 'node:assert/strict'
import test from 'node:test'
import { expandResourceTree } from './resourceExpansion'

test('expand loads newly discovered levels, skips leaves and terminates on repeated IDs', async () => {
  const rows = [{ external_id: 'space:ops', has_children: true }]
  const loaded: string[] = []
  await expandResourceTree(() => rows, async id => {
    loaded.push(id)
    if (id === 'space:ops') rows.push({ external_id: 'page:home', has_children: true })
    if (id === 'page:home') rows.push({ external_id: 'page:branch', has_children: true })
    if (id === 'page:branch') rows.push(
      { external_id: 'page:leaf', has_children: false },
      { external_id: 'space:ops', has_children: true },
    )
  }, () => false)
  assert.deepEqual(loaded, ['space:ops', 'page:home', 'page:branch'])
})

test('collapse stops expansion while a child request is in flight', async () => {
  let cancelled = false
  const rows = [{ external_id: 'root', has_children: true }]
  const loaded: string[] = []
  await expandResourceTree(() => rows, async id => {
    loaded.push(id)
    await Promise.resolve()
    rows.push({ external_id: 'child', has_children: true })
    cancelled = true
  }, () => cancelled)
  assert.deepEqual(loaded, ['root'])
})
