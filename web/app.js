'use strict'

const API = 'https://api.github.com'

const WORKERS = 6

const INTERVAL = 15

const MIN_INTERVAL = 5

const SETTINGS_KEY = 'gh-runner:settings'

const REPO_TTL = 12 * 60 * 1000

const RUNS_PER_STATUS = 20

const RATE_ATTEMPTS = 3

const RATE_BACKOFF = 3000

const MAX_REPO_ROWS = 6

const HINTS = [['enter', 'open'], ['/', 'filter'], ['h/l', 'panel'], ['r', 'rescan'], ['j/k', 'move'], ['O', 'connect']]

const CONNECT_HINTS = [['enter', 'start'], ['tab', 'next field']]

const state = {
  org: '',
  repo: '',
  days: 7,
  interval: INTERVAL,
  jobs: [],
  running: [],
  queued: [],
  focus: 0,
  cursor: [0, 0],
  filter: '',
  scanning: false,
  scanId: 0,
  progress: { done: 0, total: -1 },
  lastScan: '',
  cost: null,
  error: '',
  nextIn: INTERVAL,
  usage: { limit: 0, remaining: 0, reset: 0 },
}

let token = ''

const etags = new Map()

const repoCache = new Map()

const $ = id => document.getElementById(id)

function el(tag, cls, text) {
  const e = document.createElement(tag)
  if (cls) e.className = cls
  if (text !== undefined) e.textContent = text
  return e
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

function nextLink(header) {
  if (!header) return ''
  const m = /<([^>]+)>;\s*rel="next"/.exec(header)
  return m && m[1].startsWith(API + '/') ? m[1] : ''
}

function recordUsage(res) {
  const limit = Number(res.headers.get('x-ratelimit-limit'))
  const remaining = Number(res.headers.get('x-ratelimit-remaining'))
  const reset = Number(res.headers.get('x-ratelimit-reset'))
  if (!limit) return
  if (reset > state.usage.reset || remaining < state.usage.remaining || !state.usage.limit)
    state.usage = { limit, remaining, reset }
}

class RateLimited extends Error {}

class AuthError extends Error {}

async function getJSON(url, cost, scan) {
  if (!url.startsWith(API + '/')) throw new Error('refusing non-GitHub URL')
  for (let attempt = 0; ; attempt++) {
    const headers = {
      Accept: 'application/vnd.github+json',
      Authorization: 'Bearer ' + token,
      'X-GitHub-Api-Version': '2022-11-28',
    }
    const cached = etags.get(url)
    if (cached) headers['If-None-Match'] = cached.etag
    const res = await fetch(url, { headers, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer' })
    if (scan !== state.scanId) throw new Error('stale')
    cost.requests++
    recordUsage(res)
    if (res.status === 304 && cached) {
      cost.free++
      return { data: cached.data, next: cached.next }
    }
    if (res.ok) {
      const data = await res.json()
      const next = nextLink(res.headers.get('link'))
      const etag = res.headers.get('etag')
      if (etag) etags.set(url, { etag, data, next })
      return { data, next }
    }
    if (res.status === 401) throw new AuthError('token rejected (401)')
    const limited = res.status === 429 || (res.status === 403 && (res.headers.get('x-ratelimit-remaining') === '0' || res.headers.get('retry-after')))
    if (!limited) {
      cost.failed[res.status] = (cost.failed[res.status] || 0) + 1
      if (!cost.message) cost.message = await refusal(res)
      return null
    }
    if (attempt >= RATE_ATTEMPTS - 1) throw new RateLimited('rate limited; results are incomplete')
    const wait = Number(res.headers.get('retry-after')) * 1000
    await sleep(wait > 0 && wait <= RATE_ATTEMPTS * RATE_BACKOFF ? wait : (attempt + 1) * RATE_BACKOFF)
  }
}

async function refusal(res) {
  if (res.headers.get('x-github-sso'))
    return 'the organisation requires SSO: authorise this token for it in GitHub token settings'
  try {
    const body = await res.json()
    if (body && typeof body.message === 'string') return body.message
  } catch {}
  return `HTTP ${res.status}`
}

async function listRepos(org, days, cost, scan) {
  const key = org + '\u0000' + days
  const hit = repoCache.get(key)
  if (hit && hit.expires > Date.now()) return hit.names
  const cutoff = Date.now() - days * 24 * 3600 * 1000
  const names = []
  let next = `${API}/orgs/${encodeURIComponent(org)}/repos?per_page=100&sort=pushed&direction=desc`
  while (next) {
    const page = await getJSON(next, cost, scan)
    if (!page) throw new Error(`cannot list repos of ${org}: ${cost.message}`)
    for (const r of page.data) {
      if (r.archived) continue
      if (days <= 0 || (r.pushed_at && Date.parse(r.pushed_at) > cutoff)) names.push(r.name)
    }
    next = page.next
  }
  repoCache.set(key, { names, expires: Date.now() + REPO_TTL })
  return names
}

async function scanRepo(org, repo, cost, scan) {
  const jobs = []
  const base = `${API}/repos/${encodeURIComponent(org)}/${encodeURIComponent(repo)}/actions/runs`
  for (const status of ['in_progress', 'queued']) {
    const runs = await getJSON(`${base}?status=${status}&per_page=${RUNS_PER_STATUS}`, cost, scan)
    if (!runs) continue
    for (const run of runs.data.workflow_runs || []) {
      let next = `${base}/${encodeURIComponent(run.id)}/jobs?per_page=100`
      while (next) {
        const page = await getJSON(next, cost, scan)
        if (!page) break
        for (const j of page.data.jobs || []) {
          if (j.status === 'completed') continue
          jobs.push({
            runId: j.run_id,
            status: j.status,
            repo,
            workflow: j.workflow_name || '',
            branch: j.head_branch || '',
            name: j.name || '',
            runner: j.runner_name || '',
            labels: (j.labels || []).join(','),
            startedAt: j.started_at ? Date.parse(j.started_at) : 0,
            createdAt: j.created_at ? Date.parse(j.created_at) : 0,
            url: j.html_url || '',
          })
        }
        next = page.next
      }
    }
  }
  return jobs
}

async function scanOrg(org, days, scan) {
  const cost = { requests: 0, free: 0, failed: {}, message: '' }
  state.progress = { done: 0, total: -1 }
  render()
  const repos = state.repo ? [state.repo] : await listRepos(org, days, cost, scan)
  state.progress = { done: 0, total: repos.length }
  render()
  const results = []
  let index = 0
  let limited = null
  const worker = async () => {
    while (index < repos.length) {
      const repo = repos[index++]
      try {
        results.push(...await scanRepo(org, repo, cost, scan))
      } catch (err) {
        if (!(err instanceof RateLimited)) throw err
        limited = err
      }
      if (scan !== state.scanId) return
      state.progress.done++
      renderStatus()
    }
  }
  await Promise.all(Array.from({ length: WORKERS }, worker))
  return { jobs: results, cost, limited }
}

async function startScan() {
  if (state.scanning) return
  state.scanning = true
  const scan = ++state.scanId
  try {
    const { jobs, cost, limited } = await scanOrg(state.org, state.days, scan)
    if (scan !== state.scanId) return
    state.jobs = jobs
    state.cost = cost
    state.error = limited ? limited.message : ''
  } catch (err) {
    if (scan !== state.scanId) return
    if (err instanceof AuthError) return logout(err.message)
    state.error = err.message
  }
  state.scanning = false
  state.lastScan = new Date().toISOString().slice(11, 19)
  state.nextIn = state.interval
  refresh()
}

function logout(message) {
  token = ''
  state.scanId++
  state.scanning = false
  openConnect(message)
}

function openConnect(message) {
  $('login').hidden = false
  $('loginError').textContent = message || ''
  $('org').value = state.org
  $('repo').value = state.repo && `${state.org}/${state.repo}`
  $('token').value = ''
  $('token').required = !token
  $('token').placeholder = token ? 'keep current token' : ''
  renderHints()
  ;(state.org && token ? $('repo') : state.org ? $('token') : $('org')).focus()
}

function closeConnect() {
  $('login').hidden = true
  renderHints()
}

function target() {
  return state.repo ? `${state.org}/${state.repo}` : state.org
}

function splitRepo(org, value) {
  const repo = value.trim().replace(/^\/+|\/+$/g, '')
  const slash = repo.indexOf('/')
  if (slash < 0) return [org, repo]
  const owner = repo.slice(0, slash).trim()
  const name = repo.slice(slash + 1).trim()
  return [owner || org, name]
}

function filterJobs(jobs, needle) {
  if (!needle) return jobs
  const n = needle.toLowerCase()
  return jobs.filter(j => [j.repo, j.workflow, j.branch, j.name, j.runner, j.labels].some(f => f.toLowerCase().includes(n)))
}

function splitJobs(jobs) {
  const running = jobs.filter(j => j.status === 'in_progress').sort((a, b) => a.runner.localeCompare(b.runner))
  const queued = jobs.filter(j => j.status !== 'in_progress').sort((a, b) => a.createdAt - b.createdAt)
  return [running, queued]
}

function repoCounts(running, queued) {
  const map = new Map()
  const add = (repo, key) => {
    if (!map.has(repo)) map.set(repo, { name: repo, running: 0, queued: 0 })
    map.get(repo)[key]++
  }
  running.forEach(j => add(j.repo, 'running'))
  queued.forEach(j => add(j.repo, 'queued'))
  return [...map.values()].sort((a, b) => b.running - a.running || b.queued - a.queued || a.name.localeCompare(b.name))
}

function labelSet(labels) {
  return new Set(labels.split(',').map(l => l.trim().toLowerCase()).filter(Boolean))
}

function busyRunners(q, running) {
  const want = labelSet(q.labels)
  if (!want.size) return []
  return running.filter(r => {
    const have = labelSet(r.labels)
    return [...want].every(l => have.has(l))
  })
}

function fmtElapsed(ms) {
  const total = Math.max(0, Math.floor(ms / 1000))
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}

function waitClass(mins) {
  if (mins >= 60) return 'red'
  if (mins >= 30) return 'yellow'
  return ''
}

function utcClock(ms) {
  return new Date(ms).toISOString().slice(11, 16)
}

function safeURL(url) {
  return url.startsWith('https://github.com/') ? url : ''
}

function refresh() {
  ;[state.running, state.queued] = splitJobs(filterJobs(state.jobs, state.filter))
  state.cursor[0] = Math.min(state.cursor[0], Math.max(0, state.running.length - 1))
  state.cursor[1] = Math.min(state.cursor[1], Math.max(0, state.queued.length - 1))
  render()
}

function selected() {
  const list = state.focus === 0 ? state.running : state.queued
  return list[state.cursor[state.focus]]
}

function renderStatus() {
  const s = $('status')
  s.replaceChildren()
  if (!state.org) {
    s.append(el('div', 'dim', 'not connected'))
    return renderInfo()
  }
  const first = el('div')
  first.append(el('span', 'cyan bold', target()), el('span', 'dim', ` · ${state.progress.total >= 0 ? state.progress.total + ' repos' : '…'} · scanned ${state.lastScan || '-'} UTC`))
  let second
  if (state.scanning && state.progress.total < 0) second = el('div', 'dim', 'listing repos…')
  else if (state.scanning) second = el('div', 'dim', `scanning ${state.progress.done}/${state.progress.total} repos`)
  else if (state.cost) second = el('div', 'dim', `last scan ${state.cost.requests} req, ${state.cost.free} free (304)`)
  else second = el('div', 'dim', 'no scan yet')
  s.append(first, second)
  if (state.error) s.append(el('div', 'red', state.error))
  const failed = state.cost ? Object.entries(state.cost.failed) : []
  if (failed.length) {
    const counts = failed.map(([status, n]) => `${status} ×${n}`).join(', ')
    s.append(el('div', 'red', `${failed.reduce((t, [, n]) => t + n, 0)} requests refused (${counts}): ${state.cost.message}`))
  }
  if (!state.scanning && state.cost && state.progress.total === 0)
    s.append(el('div', 'yellow', `this token sees no repos in ${state.org} pushed in the last ${state.days} days; check the token's resource owner is the organisation`))
  renderInfo()
}

function renderInfo() {
  const info = $('info')
  info.replaceChildren()
  const u = state.usage
  if (!state.org) return
  const parts = [el('span', 'cyan bold', target()), el('span', '', ` · next ${state.scanning ? '…' : state.nextIn + 's'}`)]
  if (u.limit) {
    const cls = u.remaining < u.limit * 0.1 ? 'red' : u.remaining < u.limit * 0.3 ? 'yellow' : 'green'
    parts.push(el('span', '', ' · '), el('span', cls, `api ${u.remaining}/${u.limit}`), el('span', '', ` · resets ${new Date(u.reset * 1000).toISOString().slice(11, 16)} UTC`))
  }
  info.append(...parts)
}

function renderRepos() {
  const repos = repoCounts(state.running, state.queued)
  const box = $('repos')
  box.replaceChildren()
  $('reposFoot').textContent = repos.length ? `${repos.length} active` : ''
  if (!repos.length) return box.append(el('div', 'dim', 'no active repos'))
  for (const r of repos.slice(0, MAX_REPO_ROWS)) {
    const row = el('div', 'repo-row')
    row.append(el('span', 'name', r.name), el('span', r.running ? 'green' : 'dim', `${r.running} running`), el('span', r.queued ? 'yellow' : 'dim', `${r.queued} queued`))
    box.append(row)
  }
  if (repos.length > MAX_REPO_ROWS) box.append(el('div', 'dim', `+${repos.length - MAX_REPO_ROWS} more`))
}

function renderTable(id, focus, cols, cells) {
  const table = $(id)
  const list = focus === 0 ? state.running : state.queued
  table.replaceChildren()
  const head = el('tr')
  cols.forEach(([name, width]) => {
    const th = el('th', '', name)
    th.style.width = width
    head.append(th)
  })
  table.append(head)
  list.forEach((job, i) => {
    const tr = el('tr', 'row' + (i === state.cursor[focus] ? ' sel' : ''))
    for (const [text, cls] of cells(job)) tr.append(el('td', i === state.cursor[focus] ? '' : cls, text))
    tr.addEventListener('click', () => {
      state.focus = focus
      state.cursor[focus] = i
      render()
    })
    tr.addEventListener('dblclick', openSelected)
    table.append(tr)
  })
  const sel = table.querySelector('tr.sel')
  if (sel) sel.scrollIntoView({ block: 'nearest' })
}

function renderLists() {
  const now = Date.now()
  $('runTitle').textContent = `[1] Running (${state.running.length})`
  $('qTitle').textContent = `[2] Queued (${state.queued.length})`
  $('runPanel').classList.toggle('focused', state.focus === 0)
  $('qPanel').classList.toggle('focused', state.focus === 1)
  $('runFoot').textContent = state.running.length ? `${state.cursor[0] + 1} of ${state.running.length}` : ''
  $('qFoot').textContent = state.queued.length ? `${state.cursor[1] + 1} of ${state.queued.length}` : ''
  renderTable('runTable', 0, [['Runner', '28%'], ['Repo', '26%'], ['Job', '32%'], ['Elapsed', '14%']], j => [
    [j.runner, 'green'], [j.repo, ''], [j.name, ''], [j.startedAt ? fmtElapsed(now - j.startedAt) : '-', ''],
  ])
  renderTable('qTable', 1, [['Wait', '12%'], ['Repo', '26%'], ['Job', '32%'], ['Labels', '30%']], j => {
    const mins = j.createdAt ? Math.floor((now - j.createdAt) / 60000) : -1
    return [[mins >= 0 ? mins + 'm' : '-', waitClass(mins)], [j.repo, ''], [j.name, ''], [j.labels, '']]
  })
}

function kv(box, rows) {
  const grid = el('div', 'kv')
  for (const [k, v, cls] of rows) grid.append(el('span', 'k', k), el('span', 'v ' + (cls || ''), v))
  box.append(grid)
}

function renderDetail() {
  const box = $('detail')
  box.replaceChildren()
  const j = selected()
  $('detailPanel').querySelector('h2').textContent = j ? `Job · ${j.status === 'in_progress' ? 'running' : 'queued'}` : 'Job'
  if (!j) return box.append(el('div', 'dim', 'no job selected'))
  const now = Date.now()
  box.append(el('div', 'title', `${j.repo} › ${j.workflow} › ${j.name}`))
  const rows = [['Status', j.status === 'in_progress' ? 'running' : j.status, j.status === 'in_progress' ? 'green bold' : 'yellow bold']]
  if (j.status === 'in_progress') {
    rows.push(['Runner', j.runner || '-', 'green'])
    rows.push(['Elapsed', j.startedAt ? `${fmtElapsed(now - j.startedAt)}  (since ${utcClock(j.startedAt)} UTC)` : '-'])
  } else {
    const mins = j.createdAt ? Math.floor((now - j.createdAt) / 60000) : -1
    rows.push(['Waiting', mins >= 0 ? `${mins}m  (since ${utcClock(j.createdAt)} UTC)` : '-', waitClass(mins)])
  }
  rows.push(['Branch', j.branch], ['Labels', j.labels.split(',').join(', ')], ['Run', '#' + j.runId], ['URL', safeURL(j.url).replace('https://', ''), 'blue'])
  kv(box, rows)
  if (j.status !== 'in_progress') {
    const busy = busyRunners(j, state.running)
    const wrap = el('div', 'busy')
    wrap.append(el('div', 'dim', busy.length ? 'Busy runners with these labels' : 'No running job shares these labels: runners may be offline'))
    for (const r of busy) {
      const row = el('div', 'row')
      row.append(el('span', 'green', r.runner), el('span', '', `${r.repo} › ${r.name}`), el('span', '', r.startedAt ? fmtElapsed(now - r.startedAt) : '-'))
      wrap.append(row)
    }
    box.append(wrap)
  }
}

function renderHints() {
  const hints = $('hints')
  hints.replaceChildren()
  const list = $('login').hidden ? HINTS : token ? [...CONNECT_HINTS, ['esc', 'close']] : CONNECT_HINTS
  list.forEach(([k, v], i) => {
    if (i) hints.append(' | ')
    hints.append(el('b', '', k), `: ${v}`)
  })
}

function render() {
  renderStatus()
  renderRepos()
  renderLists()
  renderDetail()
}

function openSelected() {
  const j = selected()
  const url = j && safeURL(j.url)
  if (url) window.open(url, '_blank', 'noopener,noreferrer')
}

function move(delta) {
  const list = state.focus === 0 ? state.running : state.queued
  if (!list.length) return
  state.cursor[state.focus] = Math.max(0, Math.min(list.length - 1, state.cursor[state.focus] + delta))
  render()
}

function onKey(e) {
  if (!$('login').hidden) {
    if (e.key === 'Escape' && token) closeConnect()
    return
  }
  if (e.target === $('filter')) {
    if (e.key === 'Escape' || e.key === 'Enter') $('filter').blur()
    if (e.key === 'Escape') {
      $('filter').value = ''
      state.filter = ''
      refresh()
    }
    return
  }
  if (e.metaKey || e.ctrlKey || e.altKey) return
  const keys = {
    j: () => move(1), ArrowDown: () => move(1),
    k: () => move(-1), ArrowUp: () => move(-1),
    g: () => move(-Infinity), Home: () => move(-Infinity),
    G: () => move(Infinity), End: () => move(Infinity),
    h: () => { state.focus = 1 - state.focus; render() },
    l: () => { state.focus = 1 - state.focus; render() },
    1: () => { state.focus = 0; render() },
    2: () => { state.focus = 1; render() },
    Enter: openSelected, o: openSelected,
    r: () => startScan(),
    '/': () => $('filter').focus(),
    Escape: () => { $('filter').value = ''; state.filter = ''; refresh() },
    O: () => openConnect(),
  }
  const fn = keys[e.key]
  if (!fn) return
  e.preventDefault()
  fn()
}

function tick() {
  if (!token || state.scanning) return
  state.nextIn--
  if (state.nextIn <= 0) return startScan()
  renderInfo()
  renderLists()
  renderDetail()
}

function start(e) {
  e.preventDefault()
  const value = $('token').value.trim() || token
  const [org, repo] = splitRepo($('org').value.trim(), $('repo').value)
  if (!org || !value) return
  token = value
  $('token').value = ''
  state.scanId++
  state.scanning = false
  state.org = org
  state.repo = repo
  state.days = Math.max(0, parseInt($('days').value, 10) || 0)
  state.interval = Math.max(MIN_INTERVAL, parseInt($('interval').value, 10) || INTERVAL)
  state.nextIn = state.interval
  saveSettings()
  state.jobs = []
  state.cost = null
  state.lastScan = ''
  state.error = ''
  state.usage = { limit: 0, remaining: 0, reset: 0 }
  closeConnect()
  refresh()
  startScan()
}

function loadSettings() {
  try {
    const saved = JSON.parse(localStorage.getItem(SETTINGS_KEY) || '{}')
    if (Number.isInteger(saved.days) && saved.days >= 0) $('days').value = saved.days
    if (Number.isInteger(saved.interval) && saved.interval >= MIN_INTERVAL) $('interval').value = saved.interval
  } catch {}
}

function saveSettings() {
  try {
    localStorage.setItem(SETTINGS_KEY, JSON.stringify({ days: state.days, interval: state.interval }))
  } catch {}
}

function init() {
  loadSettings()
  renderHints()
  $('login').addEventListener('submit', start)
  $('filter').addEventListener('input', () => {
    state.filter = $('filter').value
    refresh()
  })
  document.addEventListener('keydown', onKey)
  setInterval(tick, 1000)
  refresh()
  openConnect()
}

init()
