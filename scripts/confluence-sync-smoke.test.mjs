import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import test from 'node:test'
import { run } from './confluence-sync-smoke.mjs'

test('live check distinguishes read-only, import, parse, search and repeated sync', async (t) => {
  let writes = 0
  let syncs = 0
  let failParsing = false
  let wrongSearch = false
  const sourcePage = { id: '12', title: 'Integration guide', type: 'page', status: 'current', version: { number: 2 } }
  const dataSource = { id: 'source', type: 'confluence', knowledge_base_id: 'kb', status: 'active', sync_schedule: '0 */10 * * * *', config: { resource_ids: ['page:12'], settings: { attachments: false, images: false } } }
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, 'http://localhost')
    response.setHeader('Content-Type', 'application/json')
    const send = (value) => response.end(JSON.stringify(value))
    if (url.pathname.startsWith('/rest/api')) {
      assert.equal(request.headers.authorization, 'Bearer source-test-token')
      return send(sourcePage)
    }
    assert.equal(request.headers['x-api-key'], 'target-test-key')
    if (request.method === 'POST') {
      writes++
      let body = ''
      for await (const part of request) body += part
      const payload = body ? JSON.parse(body) : {}
      if (url.pathname === '/api/v1/datasource') {
        assert.deepEqual(payload.config.resource_ids, ['page:12'])
        assert.equal(payload.sync_deletions, false)
        return send(dataSource)
      }
      if (url.pathname === '/api/v1/datasource/source/sync') return send({ id: `log-${++syncs}` })
      if (url.pathname === '/api/v1/knowledge-search') {
        assert.deepEqual(payload.knowledge_ids, ['document'])
        assert.equal(payload.knowledge_base_id, undefined)
        return send({ success: true, data: [{ knowledge_id: 'document', content: wrongSearch ? 'Old unrelated content' : 'Integration marker' }] })
      }
    }
    switch (url.pathname) {
      case '/api/v1/knowledge-bases/kb': return send({ success: true, data: { id: 'kb', type: 'document' } })
      case '/api/v1/datasource/types': return send([{ type: 'confluence' }])
      case '/api/v1/datasource/source': return send(dataSource)
      case '/api/v1/knowledge-bases/kb/knowledge': return send({ success: true, total: 2, page: Number(url.searchParams.get('page')), page_size: 1, data: url.searchParams.get('page') === '1' ? [{ id: 'unrelated', metadata: { datasource_id: 'other' } }] : [{ id: 'document', title: sourcePage.title, parse_status: failParsing ? 'failed' : 'completed', enable_status: 'enabled', metadata: { datasource_id: 'source', external_id: '12', source_version: '2' } }] })
      default:
        if (url.pathname.startsWith('/api/v1/datasource/logs/')) return send({ id: `log-${syncs}`, status: 'success', items_created: syncs === 1 ? 1 : 0, items_updated: 0, items_failed: 0 })
        response.statusCode = 404; return send({ error: 'Unknown test route' })
    }
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); return new Promise((resolve) => server.close(resolve)) })
  const base = `http://127.0.0.1:${server.address().port}`
  const config = { confluence_url: base, confluence_token: 'source-test-token', weknora_url: base, weknora_key: 'target-test-key', knowledge_base_id: 'kb', test_page_id: '12', query: 'Integration marker', attachments: false, images: false, timeout_seconds: 10 }
  const checked = await run(config, '--check', () => {})
  assert.equal(writes, 0)
  assert(!checked.stages.includes('parse_completed'))
  await assert.rejects(run(config, '--sync', () => {}), /allow_test_writes/)
  assert.equal(writes, 0)
  const imported = await run({ ...config, allow_test_writes: true }, '--sync', () => {})
  assert(imported.stages.includes('parse_completed') && imported.stages.includes('search') && imported.stages.includes('idempotency'))
  assert.equal(imported.documents[0].version, '2')
  wrongSearch = true
  await assert.rejects(run({ ...config, data_source_id: 'source', allow_test_writes: true }, '--sync', () => {}), /distinctive marker/)
  wrongSearch = false
  failParsing = true
  await assert.rejects(run({ ...config, data_source_id: 'source', allow_test_writes: true }, '--sync', () => {}), /parser\/model\/storage/)
})
