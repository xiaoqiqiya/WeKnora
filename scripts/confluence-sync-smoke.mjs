// Live integration check. Keys stay in a git-ignored local JSON file.
import assert from 'node:assert/strict'
import { readFile, writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { setTimeout as pause } from 'node:timers/promises'
import { pathToFileURL } from 'node:url'

export async function run(config, mode = '--check', progress = console.log) {
  assert(['--check', '--sync', '--watch'].includes(mode), 'Use --check, --sync or --watch')
  for (const key of ['confluence_url', 'confluence_token', 'weknora_url', 'weknora_key', 'knowledge_base_id', 'test_page_id']) {
    assert(typeof config[key] === 'string' && config[key].trim(), `Configure ${key}`)
  }
  assert(/^\d+$/.test(config.test_page_id), 'test_page_id must be numeric')
  for (const key of ['confluence_url', 'weknora_url']) {
    const url = new URL(config[key])
    assert(['http:', 'https:'].includes(url.protocol) && !url.username && !url.password && !url.search && !url.hash, `Invalid ${key}`)
  }
  const sourceAuth = config.confluence_username
    ? { Authorization: `Basic ${Buffer.from(`${config.confluence_username}:${config.confluence_token}`).toString('base64')}` }
    : { Authorization: `Bearer ${config.confluence_token}` }
  const targetAuth = { 'X-API-Key': config.weknora_key }
  async function request(base, path, auth, method = 'GET', body, envelope = false) {
    const response = await fetch(base.replace(/\/+$/, '') + path, {
      method, headers: { ...auth, Accept: 'application/json', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: 'error', signal: AbortSignal.timeout(30000),
    })
    assert(response.ok, `${method} ${path.split('?')[0]}: HTTP ${response.status}`)
    assert(response.headers.get('content-type')?.includes('application/json'), `${path.split('?')[0]} returned non-JSON; check endpoint/authentication`)
    const value = await response.json()
    assert(value.success !== false, `${path.split('?')[0]} reported failure`)
    return envelope ? value : value.data ?? value
  }
  const source = (path) => request(config.confluence_url, `/rest/api${path}`, sourceAuth)
  const target = (path, method, body, envelope) => request(config.weknora_url, `/api/v1${path}`, targetAuth, method, body, envelope)
  const array = (value) => { assert(Array.isArray(value), 'API collection shape changed'); return value }
  const id = encodeURIComponent(config.knowledge_base_id)
  progress('Checking Confluence page and WeKnora knowledge base')
  const page = await source(`/content/${config.test_page_id}?expand=body.storage,body.export_view,version,space,ancestors`)
  assert(page.id === config.test_page_id && page.type === 'page' && page.status === 'current' && page.version?.number > 0, 'Source is not a current readable page')
  const kb = await target(`/knowledge-bases/${id}`)
  assert(kb.id === config.knowledge_base_id && kb.type !== 'faq', 'Select a dedicated document knowledge base')
  const connectorTypes = array(await target('/datasource/types'))
  assert(connectorTypes.some((type) => type.type === 'confluence'), 'Deployed service does not expose Confluence')
  const report = { mode, checked_at: new Date().toISOString(), source_page_id: page.id, source_version: page.version.number, knowledge_base_id: kb.id, data_source_id: config.data_source_id || null, stages: ['source_read', 'target_kb_read', 'connector_metadata'] }
  if (mode === '--check') {
    progress('Read-only endpoint checks passed; connector metadata alone does not prove import works')
    return report
  }
  assert(config.allow_test_writes === true, 'Set allow_test_writes=true only for the dedicated test knowledge base')
  if (!config.data_source_id) {
    assert(mode === '--sync', '--watch requires an existing data_source_id')
    const created = await target('/datasource', 'POST', {
      name: `Confluence integration test ${page.id}`, type: 'confluence', knowledge_base_id: kb.id,
      config: { credentials: { base_url: config.confluence_url, api_token: config.confluence_token, username: config.confluence_username || '' }, resource_ids: [`page:${page.id}`], settings: { attachments: config.attachments !== false, images: config.images !== false } },
      sync_schedule: '0 */10 * * * *', sync_mode: 'incremental', conflict_strategy: 'overwrite', sync_deletions: false, status: 'active',
    })
    assert(created.id, 'Data source creation returned no ID')
    report.data_source_id = created.id
    progress(`Data source created: ${created.id}; reuse this ID as config.data_source_id before retrying`)
  }
  const ds = await target(`/datasource/${encodeURIComponent(report.data_source_id)}`)
  assert(ds.type === 'confluence' && ds.knowledge_base_id === kb.id, 'Data source does not belong to the selected target')
  assert(ds.config?.resource_ids?.length === 1 && ds.config.resource_ids[0] === `page:${page.id}`, 'Live test only operates on the selected test page subtree')
  const dsPath = `/datasource/${encodeURIComponent(ds.id)}`
  report.data_source_id = ds.id
  const timeout = Number(config.timeout_seconds ?? 900)
  assert(Number.isFinite(timeout) && timeout > 0 && timeout <= 3600, 'timeout_seconds must be 1..3600')
  const deadline = Date.now() + timeout * 1000
  async function listDocuments() {
    const rows = []
    const seen = new Set()
    for (let n = 1; ; n++) {
      const result = await target(`/knowledge-bases/${id}/knowledge?page=${n}&page_size=100&source=confluence`, 'GET', undefined, true)
      const batch = array(result.data)
      assert(Number.isInteger(result.total) && result.total >= 0 && result.page === n && result.page_size > 0, 'Target pagination metadata changed')
      for (const row of batch) {
        assert(row.id && !seen.has(row.id), 'Target pagination returned duplicate documents')
        seen.add(row.id)
      }
      rows.push(...batch.filter((k) => k.metadata?.datasource_id === ds.id))
      if (seen.size >= result.total) return rows
      assert(batch.length, 'Target pagination ended before its reported total')
    }
  }
  async function waitLog(logId) {
    while (Date.now() < deadline) {
      const log = await target(`/datasource/logs/${encodeURIComponent(logId)}`)
      if (['success', 'partial', 'failed', 'cancelled', 'canceled'].includes(log.status)) {
        report.latest_sync = { id: log.id, status: log.status, created: log.items_created, updated: log.items_updated, skipped: log.items_skipped, failed: log.items_failed }
        assert(log.status === 'success' && !log.items_failed, `Sync ${log.id}: ${log.status}; inspect its sync log for the environment/parse error`)
        return log
      }
      await pause(2000)
    }
    throw new Error('Timed out waiting for sync worker; check deployed version, queue and worker health')
  }
  async function search(rows) {
    assert(typeof config.query === 'string' && config.query.trim(), 'Configure a distinctive query from the test page or its image/attachment')
    const documentIds = rows.map((k) => k.id)
    const hits = array(await target('/knowledge-search', 'POST', { query: config.query, knowledge_ids: documentIds }))
    const normalize = (text) => String(text ?? '').replace(/\s+/g, ' ').trim().toLowerCase()
    assert(hits.some((hit) => documentIds.includes(hit.knowledge_id) && normalize(hit.content).includes(normalize(config.query))), 'Search did not return the distinctive marker from the imported documents; check parser/OCR/index content')
    report.search_hit_count = hits.length
    report.stages.push('search')
  }
  const expectedAttachments = []
  if (ds.config?.settings?.attachments !== false) {
    for (let offset = 0; ; ) {
      const result = await source(`/content/${page.id}/child/attachment?start=${offset}&limit=100&expand=version,metadata`)
      const batch = array(result.results)
      expectedAttachments.push(...batch.filter((a) => ds.config?.settings?.images !== false || !a.metadata?.mediaType?.startsWith('image/')))
      if (!result._links?.next) break
      const next = Number(new URL(result._links.next, config.confluence_url).searchParams.get('start'))
      assert(next > offset && batch.length, 'Source attachment pagination did not advance')
      offset = next
    }
  }
  async function readyRows() {
    const rows = await listDocuments()
    for (const row of rows) assert(!['failed', 'cancelled'].includes(row.parse_status), `Knowledge ${row.id} is ${row.parse_status}; inspect parser/model/storage configuration`)
    const expected = [page.id, ...expectedAttachments.map((a) => `${page.id}#attachment#${a.id}`)]
    const copies = expected.map((externalId) => rows.filter((k) => k.metadata?.external_id === externalId))
    for (const matches of copies) assert(matches.length <= 1, 'Duplicate source identity in the target knowledge base')
    if (copies.some((matches) => matches.length !== 1 || matches[0].parse_status !== 'completed' || matches[0].enable_status !== 'enabled')) return null
    const current = copies.map((matches) => matches[0])
    if (current[0].metadata.source_version !== String(page.version.number) || current[0].title !== page.title) return null
    for (let n = 0; n < expectedAttachments.length; n++) if (current[n+1].metadata.source_version !== String(expectedAttachments[n].version.number)) return null
    if (rows.some((k) => k.metadata?.replacement_for)) return null
    return current
  }
  if (mode === '--watch') {
    assert(ds.status === 'active' && ds.sync_schedule, 'Scheduled sync is not active')
    const startedAt = Date.now()
    progress('Waiting for an automatic sync; no manual sync request will be sent')
    while (Date.now() < deadline) {
      const logs = array(await target(`${dsPath}/logs?limit=10&offset=0`))
      const scheduled = logs.find((log) => Date.parse(log.started_at) >= startedAt && ['success', 'partial', 'failed'].includes(log.status))
      if (scheduled) {
        await waitLog(scheduled.id)
        const rows = await readyRows()
        if (rows) { await search(rows); report.stages.push('automatic_sync', 'parse_completed'); return report }
      }
      await pause(5000)
    }
    throw new Error('No usable automatic sync before timeout; check scheduler, queue, parser and model configuration')
  }
  progress('Importing the selected test page and checking parse/index completion')
  while (Date.now() < deadline) {
    const log = await target(`${dsPath}/sync`, 'POST')
    await waitLog(log.id)
    const rows = await readyRows()
    if (rows) {
      await search(rows)
      const check = await target(`${dsPath}/sync`, 'POST')
      const repeated = await waitLog(check.id)
      assert(!repeated.items_created && !repeated.items_updated, 'Unchanged content was imported again; use a stable test page to check idempotency')
      report.documents = rows.map((k) => ({ id: k.id, external_id: k.metadata.external_id, parse_status: k.parse_status, version: k.metadata.source_version }))
      report.stages.push('manual_sync', 'parse_completed', 'idempotency')
      progress('Import, indexing, search and repeated-sync checks passed')
      return report
    }
    progress('Waiting for parsing or safe replacement; next sync will reconcile completed candidates')
    await pause(5000)
  }
  throw new Error('Import/replacement did not converge before timeout; inspect sync logs and knowledge parse states')
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const path = process.argv[2]
    assert(path, 'Usage: node scripts/confluence-sync-smoke.mjs <local-config.json> [--check|--sync|--watch]')
    const config = JSON.parse(await readFile(path, 'utf8'))
    const report = await run(config, process.argv[3] ?? '--check')
    const reportPath = resolve('.confluence-sync-test-report.json')
    await writeFile(reportPath, JSON.stringify(report, null, 2) + '\n', { mode: 0o600 })
    console.log(`Evidence saved: ${reportPath}`)
    console.log(`Data source ID: ${report.data_source_id ?? '(read-only check)'}`)
  } catch (error) {
    // Error bodies and configuration values are never dumped.
    console.error(`Integration check failed: ${error.message}`)
    process.exitCode = 1
  }
}
