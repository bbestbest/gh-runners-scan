'use strict'

const API = 'https://api.github.com'

const WORKERS = 6

const REPO_WORKERS = 4 * WORKERS

const INTERVAL = 15

const MIN_INTERVAL = 5

const SETTINGS_KEY = 'gh-runner:settings'

const TOKEN_KEY = 'gh-runner:token'

const SESSION_KEY = 'gh-runner:session'

const PM_USER = 'gh-runner'

const LEGACY_VIEW_KEY = 'gh-runner:view'

const KEY_DB = 'gh-runner'

const KDF_ITERATIONS = 600000

const REPO_TTL = 12 * 60 * 1000

const SWEEP_BUCKETS = 8

const WORKFLOWS_TTL = 24 * 60 * 60 * 1000

const ROUNDS_MAX_AGE = 30 * 60 * 1000

const FRESH_WINDOW = 24 * 60 * 60 * 1000

const CACHE_VERSION = 1

const RUNS_PER_STATUS = 20

const RATE_ATTEMPTS = 3

const RATE_BACKOFF = 3000

const MAX_REPO_ROWS = 6

const HINTS = [['enter', 'open'], ['/', 'filter'], ['h/l', 'panel'], ['r', 'rescan'], ['j/k', 'move'], ['O', 'choose']]

const CONNECT_HINTS = [['enter', 'start'], ['space', 'tick'], ['tab', 'next field']]

const state = {
  targets: [],
  pending: null,
  repoCount: -1,
  unseen: 0,
  stale: false,
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

let account = {}

let vault = null

let repoSeq = 0

let accounts = []

const picks = new Map()

const repoChoices = new Map()

let openOwner = ''

let lastResults = null

let repoFilter = ''

let daysTimer = 0

const etags = new Map()

const repoCache = new Map()

const usedUrls = new Set()

const seenRepos = new Map()

const repoHints = new Set()

const repoWorkflows = new Map()

let roundCount = 0

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

let freeSlots = WORKERS

const slotQueue = []

function takeSlot() {
  if (freeSlots > 0) {
    freeSlots--
    return Promise.resolve()
  }
  return new Promise(resolve => slotQueue.push(resolve))
}

function giveSlot() {
  const next = slotQueue.shift()
  if (next) next()
  else freeSlots++
}

async function getJSON(url, cost, scan) {
  if (!url.startsWith(API + '/')) throw new Error('refusing non-GitHub URL')
  for (let attempt = 0; ; attempt++) {
    const headers = {
      Accept: 'application/vnd.github+json',
      Authorization: 'Bearer ' + token,
      'X-GitHub-Api-Version': '2022-11-28',
    }
    const cached = etags.get(url)
    if (cached) usedUrls.add(url)
    if (cached) headers['If-None-Match'] = cached.etag
    await takeSlot()
    let res
    try {
      res = await fetch(url, { headers, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer' })
    } finally {
      giveSlot()
    }
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
      if (etag) {
        etags.set(url, { etag, data: trimBody(data), next })
        usedUrls.add(url)
      }
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
    return 'SSO is required: authorise this token for it in GitHub token settings'
  try {
    const body = await res.json()
    if (body && typeof body.message === 'string') return body.message
  } catch {}
  return `HTTP ${res.status}`
}

async function reposURL(owner, get) {
  const name = encodeURIComponent(owner)
  if (account.login && owner.toLowerCase() === account.login.toLowerCase())
    return `${API}/user/repos?per_page=100&sort=pushed&direction=desc&affiliation=owner`
  const info = await get(`${API}/users/${name}`)
  if (!info) return ''
  if (info.type === 'Organization') return `${API}/orgs/${name}/repos?per_page=100&sort=pushed&direction=desc`
  return `${API}/users/${name}/repos?per_page=100&sort=pushed&direction=desc&type=owner`
}

function cachedRepos(owner, days) {
  const hit = repoCache.get(owner.toLowerCase())
  if (!hit || Date.now() - hit.fetched > REPO_TTL || !(hit.days === 0 || (days > 0 && days <= hit.days))) return null
  const cutoff = Date.now() - days * 24 * 3600 * 1000
  return hit.repos.filter(r => days <= 0 || r.pushed > cutoff)
}

function storeRepos(owner, days, repos) {
  repoCache.set(owner.toLowerCase(), { repos, days, fetched: Date.now() })
}

async function fetchRepos(org, days, cost, scan) {
  const cutoff = Date.now() - days * 24 * 3600 * 1000
  const repos = []
  let next = await reposURL(org, async url => {
    const page = await getJSON(url, cost, scan)
    return page && page.data
  })
  if (!next) throw new Error(`cannot find ${org}: ${cost.message}`)
  while (next) {
    const page = await getJSON(next, cost, scan)
    if (!page) throw new Error(`cannot list repos of ${org}: ${cost.message}`)
    let older = false
    for (const r of page.data) {
      if (r.archived) continue
      const pushed = r.pushed_at ? Date.parse(r.pushed_at) : 0
      if (days > 0 && !pushed) continue
      if (days > 0 && pushed <= cutoff) {
        older = true
        break
      }
      repos.push({ name: r.name, pushed })
    }
    next = older ? '' : page.next
  }
  storeRepos(org, days, repos)
  return repos
}

async function scanRepo(org, repo, cost, scan) {
  const base = `${API}/repos/${encodeURIComponent(org)}/${encodeURIComponent(repo)}/actions/runs`
  const lists = await Promise.all(['in_progress', 'queued'].map(status => getJSON(`${base}?status=${status}&per_page=${RUNS_PER_STATUS}`, cost, scan)))
  const runs = lists.flatMap(page => (page && page.data.workflow_runs) || [])
  const perRun = await Promise.all(runs.map(run => runJobs(org, repo, `${base}/${encodeURIComponent(run.id)}/jobs?per_page=100`, cost, scan)))
  return perRun.flat()
}

async function runJobs(org, repo, next, cost, scan) {
  const jobs = []
  while (next) {
    const page = await getJSON(next, cost, scan)
    if (!page) break
    for (const j of page.data.jobs || []) {
      if (j.status === 'completed') continue
      jobs.push({
        runId: j.run_id,
        status: j.status,
        owner: org,
        repo,
        workflow: j.workflow_name || '',
        branch: j.head_branch || '',
        name: j.name || '',
        runner: j.runner_name || '',
        labels: (j.labels || []).join(','),
        startedAt: j.started_at ? Date.parse(j.started_at) : 0,
        createdAt: j.created_at ? Date.parse(j.created_at) : 0,
        url: j.html_url || '',
        step: currentStep(j.steps || []),
      })
    }
    next = page.next
  }
  return jobs
}

function currentStep(steps) {
  const i = steps.findIndex(s => s.status !== 'completed')
  if (i >= 0) return { num: i + 1, total: steps.length, name: steps[i].name || '' }
  if (!steps.length) return { num: 0, total: 0, name: '' }
  return { num: steps.length, total: steps.length, name: steps[steps.length - 1].name || '' }
}

function stepFraction(step) {
  return step && step.total ? `${step.num}/${step.total}` : '-'
}

function windowTitle(jobs) {
  const queued = jobs.filter(j => j.status !== 'in_progress').length
  return queued ? `(${queued} queued) gh-runner` : 'gh-runner'
}

function sweepBucket(key) {
  let h = 0x811c9dc5
  for (const c of new TextEncoder().encode(key)) h = Math.imul(h ^ c, 0x01000193) >>> 0
  return h % SWEEP_BUCKETS
}

function needsScan(key, pushed, round) {
  const m = seenRepos.get(key)
  const inBucket = sweepBucket(key) === round % SWEEP_BUCKETS
  if (!m) return repoHints.has(key) || !pushed || Date.now() - pushed <= FRESH_WINDOW || inBucket
  return m.active || !pushed || pushed > m.pushed || inBucket
}

async function hasWorkflows(owner, repo, pushed, cost, scan) {
  const key = `${owner}/${repo}`.toLowerCase()
  const w = repoWorkflows.get(key)
  if (w && Date.now() - w.checked <= WORKFLOWS_TTL && !(pushed > w.checked)) return w.count > 0
  let count = 1
  try {
    const page = await getJSON(`${API}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/workflows?per_page=1`, cost, scan)
    if (page && Number.isInteger(page.data.total_count)) count = page.data.total_count
  } catch (err) {
    if (err instanceof RateLimited) return true
    throw err
  }
  repoWorkflows.set(key, { count, checked: Date.now() })
  return count > 0
}

function replaceRepoJobs(jobs, owner, repo, fresh) {
  const same = j => j.owner.toLowerCase() === owner.toLowerCase() && j.repo.toLowerCase() === repo.toLowerCase()
  return [...jobs.filter(j => !same(j)), ...fresh]
}

async function scanTargets(targets, days, scan) {
  const cost = { requests: 0, free: 0, failed: {}, message: '' }
  state.progress = { done: 0, total: -1 }
  render()
  const round = roundCount++
  const pairs = []
  for (const t of targets) {
    if (!t.all) {
      for (const repo of t.repos) pairs.push({ owner: t.owner, repo, pushed: 0 })
      continue
    }
    for (const r of await fetchRepos(t.owner, days, cost, scan)) pairs.push({ owner: t.owner, repo: r.name, pushed: r.pushed })
  }
  const todo = pairs.filter(p => needsScan(`${p.owner}/${p.repo}`.toLowerCase(), p.pushed, round))
  const chosen = new Set(todo.map(p => `${p.owner}/${p.repo}`.toLowerCase()))
  state.unseen = pairs.filter(p => {
    const key = `${p.owner}/${p.repo}`.toLowerCase()
    return !chosen.has(key) && !seenRepos.has(key)
  }).length
  state.progress = { done: pairs.length - todo.length, total: pairs.length }
  state.repoCount = pairs.length
  render()
  const results = []
  let index = 0
  let limited = null
  const worker = async () => {
    while (index < todo.length) {
      const { owner, repo, pushed } = todo[index++]
      try {
        const jobs = (await hasWorkflows(owner, repo, pushed, cost, scan)) ? await scanRepo(owner, repo, cost, scan) : []
        results.push(...jobs)
        if (scan === state.scanId && !limited) seenRepos.set(`${owner}/${repo}`.toLowerCase(), { pushed, active: jobs.length > 0 })
        if (scan === state.scanId) {
          state.jobs = replaceRepoJobs(state.jobs, owner, repo, jobs)
          refresh()
        }
      } catch (err) {
        if (!(err instanceof RateLimited)) throw err
        limited = err
      }
      if (scan !== state.scanId) return
      state.progress.done++
      renderStatus()
    }
  }
  await Promise.all(Array.from({ length: REPO_WORKERS }, worker))
  return { jobs: results, cost, limited }
}

async function startScan() {
  if (state.scanning) return
  if (state.pending) applyPending()
  if (!state.targets.length) return
  state.scanning = true
  const scan = ++state.scanId
  try {
    const { jobs, cost, limited } = await scanTargets(state.targets, state.days, scan)
    if (scan !== state.scanId) return
    state.jobs = jobs
    document.title = windowTitle(jobs)
    state.cost = cost
    state.error = limited ? limited.message : ''
    state.stale = false
    if (!limited) lastResults = { key: viewKey(), at: Date.now(), jobs }
  } catch (err) {
    if (scan !== state.scanId) return
    if (err instanceof AuthError) return logout(err.message)
    state.error = err.message
  }
  state.scanning = false
  state.lastScan = clock(Date.now(), true)
  state.nextIn = state.interval
  refresh()
  saveCache()
}

function logout(message) {
  token = ''
  state.scanId++
  state.scanning = false
  account = {}
  forgetToken()
  dropSession()
  openConnect(message)
}

function openConnect(message) {
  $('login').hidden = false
  $('loginError').textContent = message || ''
  if (token) showConfig()
  else if (savedToken()) showUnlock()
  else showTokenStep()
  renderHints()
}

function showUnlock() {
  const saved = savedToken()
  $('stepUnlock').hidden = false
  $('stepToken').hidden = true
  $('stepConfig').hidden = true
  $('connectTitle').textContent = 'Connect · unlock saved token'
  $('submit').textContent = 'enter: unlock'
  $('savedFor').textContent = `token saved for ${saved && saved.login ? saved.login : 'this device'}`
  $('unlockPass').value = ''
  $('unlockPass').focus()
}

function showTokenStep() {
  $('stepUnlock').hidden = true
  $('stepToken').hidden = false
  $('stepConfig').hidden = true
  $('remember').checked = !!savedToken()
  $('rememberFields').hidden = !$('remember').checked
  $('connectTitle').textContent = 'Connect · 1/2 token'
  $('submit').textContent = 'enter: continue'
  $('token').value = ''
  $('token').focus()
}

function showConfig() {
  $('stepUnlock').hidden = true
  $('stepToken').hidden = true
  $('stepConfig').hidden = false
  $('connectTitle').textContent = 'Connect · 2/2 what to scan'
  $('submit').textContent = 'enter: start'
  $('whoami').textContent = account.login ? `signed in as ${account.login}${account.scopes ? ` · scopes: ${account.scopes}` : ''}` : ''
  $('whoami').className = account.scopes && !account.scopes.split(/,\s*/).includes('repo') ? 'yellow' : 'dim'
  if ($('whoami').className === 'yellow') $('whoami').textContent += ' · no repo scope: private repos stay hidden'
  $('org').value = ''
  setPicks((state.pending || state).targets)
  renderAccounts()
  const first = $('accounts').querySelector('input')
  ;(first || $('org')).focus()
  renderHints()
}

function closeConnect() {
  $('login').hidden = true
  renderHints()
}

function toggleConnect() {
  if ($('login').hidden) openConnect()
  else if (token) closeConnect()
}

async function connectGet(url) {
  if (!url.startsWith(API + '/')) throw new Error('refusing non-GitHub URL')
  const res = await fetch(url, {
    headers: { Accept: 'application/vnd.github+json', Authorization: 'Bearer ' + token, 'X-GitHub-Api-Version': '2022-11-28' },
    cache: 'no-store',
    credentials: 'omit',
    referrerPolicy: 'no-referrer',
    signal: AbortSignal.timeout(15000),
  }).catch(err => {
    throw new Error(err.name === 'TimeoutError' ? 'GitHub did not answer within 15s' : 'cannot reach api.github.com')
  })
  recordUsage(res)
  if (res.status === 401) throw new AuthError('token rejected (401): check it was copied whole and has not expired')
  if (!res.ok) throw new Error(await refusal(res))
  return { res, data: await res.json(), next: nextLink(res.headers.get('link')) }
}

function setBusy(label) {
  const button = $('submit')
  button.disabled = !!label
  button.classList.toggle('busy', !!label)
  button.textContent = label || (!$('stepUnlock').hidden ? 'enter: unlock' : $('stepConfig').hidden ? 'enter: continue' : 'enter: start')
}

async function verifyToken(value) {
  token = value
  $('loginError').textContent = ''
  setBusy('checking token…')
  try {
    const { res, data } = await connectGet(`${API}/user`)
    account = { login: data.login, scopes: res.headers.get('x-oauth-scopes') || '' }
    setBusy('loading accounts…')
    accounts = [{ login: data.login, kind: 'you' }]
    try {
      const { data: list } = await connectGet(`${API}/user/orgs?per_page=100`)
      for (const o of list) accounts.push({ login: o.login, kind: 'org' })
    } catch {}
    return 'ok'
  } catch (err) {
    token = ''
    account = {}
    $('loginError').textContent = err.message
    return err instanceof AuthError ? 'rejected' : 'error'
  } finally {
    setBusy('')
  }
}

async function submitToken() {
  const value = $('token').value.trim()
  const remember = $('remember').checked
  const pass = $('newPass').value
  if (!value) {
    $('loginError').textContent = 'paste a token first'
    return $('token').focus()
  }
  if (remember && pass.length < 8) {
    $('loginError').textContent = 'the passphrase needs at least 8 characters'
    return $('newPass').focus()
  }
  $('token').disabled = true
  $('newPass').disabled = true
  const ok = await verifyToken(value) === 'ok'
  if (ok && !remember) {
    forgetToken()
    dropSession()
  }
  if (ok && remember) {
    setBusy('encrypting token…')
    try {
      await saveToken(value, pass, account.login)
      await keepSession(value)
      await loadCache()
      await offerToPasswordManager(pass)
    } catch (err) {
      $('loginError').textContent = `token works but could not be saved: ${err.message}`
    }
    setBusy('')
  }
  $('token').disabled = false
  $('newPass').disabled = false
  if (!ok) return $('token').focus()
  $('token').value = ''
  $('newPass').value = ''
  $('newPass').type = 'password'
  $('passNote').textContent = ''
  showConfig()
}

async function submitUnlock() {
  const saved = savedToken()
  const pass = $('unlockPass').value
  if (!saved) return showTokenStep()
  if (!pass) {
    $('loginError').textContent = 'type the passphrase'
    return $('unlockPass').focus()
  }
  $('unlockPass').disabled = true
  setBusy('unlocking…')
  let opened = null
  try {
    opened = await openToken(saved, pass)
  } catch {
    $('loginError').textContent = 'wrong passphrase'
  }
  setBusy('')
  $('unlockPass').disabled = false
  if (!opened) {
    $('unlockPass').select()
    return
  }
  $('unlockPass').value = ''
  const result = await verifyToken(opened.token)
  if (result === 'ok') {
    await loadCache()
    const view = await latestView(opened.view)
    await keepSession(opened.token, view)
    return applyView(view) || showConfig()
  }
  if (result === 'rejected') {
    forgetToken()
    dropSession()
    showTokenStep()
    $('loginError').textContent = 'the saved token was rejected (401) and has been forgotten; paste a new one'
  }
}

const b64 = bytes => btoa(String.fromCharCode(...new Uint8Array(bytes)))

const unb64 = text => Uint8Array.from(atob(text), c => c.charCodeAt(0))

async function deriveKey(passphrase, salt) {
  const base = await crypto.subtle.importKey('raw', new TextEncoder().encode(passphrase), 'PBKDF2', false, ['deriveKey'])
  return crypto.subtle.deriveKey({ name: 'PBKDF2', salt, iterations: KDF_ITERATIONS, hash: 'SHA-256' }, base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt'])
}

async function saveToken(value, passphrase, login) {
  const salt = crypto.getRandomValues(new Uint8Array(16))
  vault = { key: await deriveKey(passphrase, salt), salt: b64(salt), login }
  await sealVault(value, null)
}

async function sealVault(value, view) {
  if (!vault) return
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const data = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, vault.key, new TextEncoder().encode(JSON.stringify({ token: value, view })))
  localStorage.setItem(TOKEN_KEY, JSON.stringify({ v: 2, login: vault.login, salt: vault.salt, iv: b64(iv), data: b64(data) }))
}

async function openToken(saved, passphrase) {
  const key = await deriveKey(passphrase, unb64(saved.salt))
  const plain = new TextDecoder().decode(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(saved.iv) }, key, unb64(saved.data)))
  vault = { key, salt: saved.salt, login: saved.login }
  if (saved.v === 1) return { token: plain, view: null }
  const payload = JSON.parse(plain)
  return { token: payload.token, view: payload.view || null }
}

function savedToken() {
  try {
    const saved = JSON.parse(localStorage.getItem(TOKEN_KEY) || 'null')
    return saved && (saved.v === 1 || saved.v === 2) && saved.salt && saved.iv && saved.data ? saved : null
  } catch {
    return null
  }
}

async function suggestPassphrase() {
  const pass = b64(crypto.getRandomValues(new Uint8Array(18))).replace(/\+/g, '-').replace(/\//g, '_')
  const field = $('newPass')
  field.value = pass
  field.type = 'text'
  field.focus()
  field.select()
  $('passNote').textContent = 'shown above; save it in your password manager before you continue'
  try {
    await navigator.clipboard.writeText(pass)
    $('passNote').textContent = 'copied; save it in your password manager before you continue'
  } catch {}
}

async function offerToPasswordManager(pass) {
  if (!window.PasswordCredential || !navigator.credentials) return
  try {
    await navigator.credentials.store(new PasswordCredential({ id: PM_USER, password: pass, name: 'gh-runner passphrase' }))
  } catch {}
}

async function passwordFromManager() {
  if (!window.PasswordCredential || !navigator.credentials) return ''
  try {
    const cred = await navigator.credentials.get({ password: true, mediation: 'optional' })
    return cred && cred.password ? cred.password : ''
  } catch {
    return ''
  }
}

function keyStore(mode, run, name = 'keys') {
  return new Promise((resolve, reject) => {
    const open = indexedDB.open(KEY_DB, 2)
    open.onupgradeneeded = () => {
      for (const store of ['keys', 'cache'])
        if (!open.result.objectStoreNames.contains(store)) open.result.createObjectStore(store)
    }
    open.onerror = () => reject(open.error)
    open.onsuccess = () => {
      const tx = open.result.transaction(name, mode)
      const req = run(tx.objectStore(name))
      tx.oncomplete = () => resolve(req.result)
      tx.onerror = () => reject(tx.error)
    }
  })
}

async function sessionKey() {
  const existing = await keyStore('readonly', store => store.get('session'))
  if (existing) return existing
  const key = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt'])
  await keyStore('readwrite', store => store.put(key, 'session'))
  return key
}

function currentView() {
  const view = state.pending || state
  return { targets: view.targets, days: view.days, interval: state.interval }
}

async function keepSession(value, view) {
  try {
    const key = await sessionKey()
    const iv = crypto.getRandomValues(new Uint8Array(12))
    const data = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, new TextEncoder().encode(JSON.stringify({ token: value, view: view || null })))
    sessionStorage.setItem(SESSION_KEY, JSON.stringify({ iv: b64(iv), data: b64(data) }))
  } catch {}
}

async function readSession() {
  try {
    const raw = JSON.parse(sessionStorage.getItem(SESSION_KEY) || 'null')
    if (!raw || !raw.iv || !raw.data) return null
    const key = await sessionKey()
    const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(raw.iv) }, key, unb64(raw.data))
    const payload = JSON.parse(new TextDecoder().decode(plain))
    return typeof payload.token === 'string' && payload.token ? payload : null
  } catch {
    return null
  }
}

function hasSession() {
  try {
    return sessionStorage.getItem(SESSION_KEY) !== null
  } catch {
    return false
  }
}

function dropSession() {
  try {
    sessionStorage.removeItem(SESSION_KEY)
    sessionStorage.removeItem(LEGACY_VIEW_KEY)
  } catch {}
}

function viewTargets(view) {
  if (!view) return []
  const name = v => typeof v === 'string' && v !== ''
  if (Array.isArray(view.targets))
    return view.targets
      .filter(t => t && name(t.owner) && (t.all === true || (Array.isArray(t.repos) && t.repos.length && t.repos.every(name))))
      .map(t => ({ owner: t.owner, all: t.all === true, repos: t.all === true ? [] : [...t.repos] }))
  if (name(view.org)) return [{ owner: view.org, all: !name(view.repo), repos: name(view.repo) ? [view.repo] : [] }]
  return []
}

function applyView(view) {
  const targets = viewTargets(view)
  if (!targets.length) return false
  setPicks(targets)
  if (Number.isInteger(view.days) && view.days >= 0) $('days').value = view.days
  if (Number.isInteger(view.interval) && view.interval >= MIN_INTERVAL) $('interval').value = view.interval
  start()
  return true
}

function statusNote(text) {
  $('status').replaceChildren(el('div', 'dim', text))
}

async function resumeSession() {
  const saved = savedToken()
  if (!saved) {
    dropSession()
    return openConnect()
  }
  let opened = await readSession()
  const fromSession = !!opened
  if (!opened) {
    dropSession()
    const pass = await passwordFromManager()
    if (!pass) return openConnect()
    statusNote('unlocking with the saved passphrase…')
    try {
      opened = await openToken(saved, pass)
    } catch {
      return openConnect('the saved passphrase did not unlock the token; type it')
    }
  }
  statusNote('checking token…')
  const result = await verifyToken(opened.token)
  if (result === 'ok') {
    await loadCache()
    const view = fromSession ? opened.view : await latestView(opened.view)
    await keepSession(opened.token, view)
    return applyView(view) || openConnect()
  }
  if (result === 'rejected') {
    forgetToken()
    dropSession()
    return openConnect('the saved token was rejected (401) and has been forgotten; paste a new one')
  }
  openConnect($('loginError').textContent)
}

function formDays() {
  return Math.max(0, parseInt($('days').value, 10) || 0)
}

async function cacheKey(salt, info = 'gh-runner cache') {
  const base = await crypto.subtle.importKey('raw', new TextEncoder().encode(token), 'HKDF', false, ['deriveKey'])
  return crypto.subtle.deriveKey({ name: 'HKDF', hash: 'SHA-256', salt, info: new TextEncoder().encode(info) }, base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt'])
}

async function sealRecord(name, value) {
  const salt = crypto.getRandomValues(new Uint8Array(16))
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const data = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, await cacheKey(salt, 'gh-runner ' + name), new TextEncoder().encode(JSON.stringify(value)))
  await keyStore('readwrite', store => store.put({ v: CACHE_VERSION, salt: b64(salt), iv: b64(iv), data: b64(data) }, name), 'cache')
}

async function openRecord(name) {
  const raw = await keyStore('readonly', store => store.get(name), 'cache')
  if (!raw || raw.v !== CACHE_VERSION) return null
  const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(raw.iv) }, await cacheKey(unb64(raw.salt), 'gh-runner ' + name), unb64(raw.data))
  return JSON.parse(new TextDecoder().decode(plain))
}

async function saveView(view) {
  if (!token || !savedToken()) return
  try {
    await sealRecord('view', view)
  } catch {}
}

async function latestView(fallback) {
  if (!token || !savedToken()) return fallback
  try {
    const view = await openRecord('view')
    return viewTargets(view).length ? view : fallback
  } catch {
    return fallback
  }
}

function trimBody(data) {
  if (Array.isArray(data)) return data.map(r => ({ name: r.name, pushed_at: r.pushed_at, archived: r.archived }))
  if (data && Array.isArray(data.workflows)) return { total_count: data.total_count }
  if (data && Array.isArray(data.workflow_runs)) return { workflow_runs: data.workflow_runs.map(r => ({ id: r.id })) }
  if (data && Array.isArray(data.jobs))
    return {
      jobs: data.jobs.map(j => ({
        run_id: j.run_id, status: j.status, workflow_name: j.workflow_name, head_branch: j.head_branch, name: j.name,
        runner_name: j.runner_name, labels: j.labels, started_at: j.started_at, created_at: j.created_at, html_url: j.html_url,
        steps: (j.steps || []).map(st => ({ name: st.name, status: st.status })),
      })),
    }
  return { login: data && data.login, type: data && data.type }
}

async function saveCache() {
  if (!token || !savedToken()) return
  try {
    const now = Date.now()
    const payload = {
      etags: [...etags].filter(([url]) => usedUrls.has(url)),
      repos: [...repoCache].filter(([, entry]) => now - entry.fetched <= REPO_TTL),
      last: lastResults,
      rounds: { at: now, count: roundCount, seen: [...seenRepos], workflows: [...repoWorkflows] },
    }
    await sealRecord('api', payload)
  } catch {}
}

async function loadCache() {
  if (!token || !savedToken()) return
  try {
    const payload = await openRecord('api')
    if (!payload) return
    const now = Date.now()
    for (const [url, entry] of payload.etags || [])
      if (typeof url === 'string' && url.startsWith(API + '/') && entry && typeof entry.etag === 'string') etags.set(url, entry)
    for (const [key, entry] of payload.repos || [])
      if (typeof key === 'string' && entry && now - entry.fetched <= REPO_TTL && Number.isInteger(entry.days) && Array.isArray(entry.repos)) repoCache.set(key, entry)
    const last = payload.last
    if (last && typeof last.key === 'string' && Number.isFinite(last.at) && Array.isArray(last.jobs)) lastResults = last
    const rounds = payload.rounds
    if (rounds && Array.isArray(rounds.workflows))
      for (const [key, w] of rounds.workflows)
        if (typeof key === 'string' && w && Number.isInteger(w.count) && Number.isFinite(w.checked) && now - w.checked <= WORKFLOWS_TTL) repoWorkflows.set(key, w)
    if (rounds && Number.isFinite(rounds.at) && now - rounds.at <= ROUNDS_MAX_AGE && Number.isInteger(rounds.count) && Array.isArray(rounds.seen)) {
      for (const [key, m] of rounds.seen)
        if (typeof key === 'string' && m && Number.isFinite(m.pushed) && typeof m.active === 'boolean') seenRepos.set(key, m)
      roundCount = rounds.count
    }
  } catch {}
}

function dropCache() {
  seenRepos.clear()
  repoWorkflows.clear()
  repoHints.clear()
  lastResults = null
  etags.clear()
  repoCache.clear()
  usedUrls.clear()
  keyStore('readwrite', store => store.clear(), 'cache').catch(() => {})
}

function forgetToken() {
  vault = null
  dropCache()
  try {
    localStorage.removeItem(TOKEN_KEY)
  } catch {}
}

function setPicks(targets) {
  picks.clear()
  for (const t of targets) picks.set(t.owner.toLowerCase(), { owner: t.owner, all: t.all, repos: new Set(t.repos) })
}

function selection() {
  const out = []
  for (const p of picks.values()) {
    if (p.all) out.push({ owner: p.owner, all: true, repos: [] })
    else if (p.repos.size) out.push({ owner: p.owner, all: false, repos: [...p.repos].sort() })
  }
  return out
}

function pickRows() {
  const rows = [...accounts]
  const seen = new Set(rows.map(a => a.login.toLowerCase()))
  for (const [key, p] of picks)
    if (!seen.has(key)) {
      rows.push({ login: p.owner, kind: 'saved' })
      seen.add(key)
    }
  return rows
}

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function tickRow(text, checked, id, onChange) {
  const label = el('label', 'check')
  const tick = el('input')
  tick.type = 'checkbox'
  tick.checked = checked
  tick.dataset.pick = id
  tick.addEventListener('change', () => {
    onChange(tick.checked)
    renderAccounts()
  })
  label.append(tick, el('span', 'name', text))
  return label
}

function toggleRepo(owner, name, on, names) {
  const key = owner.toLowerCase()
  const p = picks.get(key) || { owner, all: false, repos: new Set() }
  if (p.all) {
    p.all = false
    p.repos = new Set(names)
  }
  if (on) p.repos.add(name)
  else p.repos.delete(name)
  if (p.repos.size) picks.set(key, p)
  else picks.delete(key)
}

function age(ms) {
  const mins = Math.max(0, Math.floor((Date.now() - ms) / 60000))
  if (mins < 60) return mins + 'm'
  if (mins < 1440) return Math.floor(mins / 60) + 'h'
  return Math.floor(mins / 1440) + 'd'
}

function leaf(branch, ...parts) {
  const row = el('div', 'leaf')
  const branchEl = el('span', 'branch ' + ({ '│': 'pipe', '├─': 'tee', '└─': 'last' }[branch] || 'none'), branch)
  branchEl.setAttribute('aria-hidden', 'true')
  row.append(branchEl, ...parts)
  return row
}

function repoList(owner) {
  const key = owner.toLowerCase()
  const wrap = el('div', 'repolist')
  const choice = repoChoices.get(key)
  const repos = (choice && choice.repos) || []
  const names = repos.map(r => r.name)
  const p = picks.get(key)
  const all = !!(p && p.all)
  const filter = el('input', 'filter')
  filter.type = 'search'
  filter.placeholder = '⌕ filter repos…'
  filter.spellcheck = false
  filter.value = repoFilter
  filter.dataset.pick = 'f:' + key
  filter.addEventListener('input', () => {
    repoFilter = filter.value
    renderAccounts()
  })
  filter.addEventListener('keydown', e => {
    if (e.key === 'Enter') e.preventDefault()
    if (e.key === 'Escape' && repoFilter) {
      e.stopPropagation()
      repoFilter = ''
      renderAccounts()
    }
  })
  wrap.append(leaf('│', filter))
  const needle = repoFilter.toLowerCase()
  const items = [[tickRow('all repositories', all, 'all:' + key, on => {
    if (on) picks.set(key, { owner, all: true, repos: new Set() })
    else picks.delete(key)
  })]]
  for (const r of repos.filter(r => r.name.toLowerCase().includes(needle)))
    items.push([
      tickRow(r.name, all || !!(p && p.repos.has(r.name)), `r:${key}/${r.name}`, on => toggleRepo(owner, r.name, on, names)),
      el('span', 'age dim', r.pushed ? 'pushed ' + age(r.pushed) : ''),
    ])
  if (needle && items.length === 1 && choice && choice.done) items.push([el('span', 'dim', `no repo matches "${repoFilter}"`)])
  items.forEach((parts, i) => wrap.append(leaf(i === items.length - 1 ? '└─' : '├─', ...parts)))
  if (choice && choice.note) wrap.append(leaf('', el('span', choice.cls, choice.note)))
  return wrap
}

function pickSummary(targets) {
  if (!targets.length) return 'nothing ticked'
  const all = targets.filter(t => t.all).length
  const repos = targets.reduce((n, t) => n + t.repos.length, 0)
  const parts = []
  if (all) parts.push(`${all} all`)
  if (repos) parts.push(plural(repos, 'repo'))
  return `${plural(targets.length, 'account')} · ${parts.join(' + ')}`
}

function renderAccounts() {
  const box = $('accounts')
  const active = document.activeElement
  const focused = active && active.dataset ? active.dataset.pick : ''
  box.replaceChildren()
  const rows = pickRows()
  if (!rows.length) box.append(el('div', 'dim', 'no accounts yet; add an owner below'))
  for (const a of rows) {
    const key = a.login.toLowerCase()
    const p = picks.get(key)
    const choice = repoChoices.get(key)
    const row = el('div', 'acct')
    const label = tickRow(a.login, !!(p && p.all), 'a:' + key, on => {
      if (on) picks.set(key, { owner: a.login, all: true, repos: new Set() })
      else picks.delete(key)
    })
    label.querySelector('input').indeterminate = !!(p && !p.all && p.repos.size)
    const open = openOwner === key
    const arrow = el('button', 'arrow', open ? '▾' : '▸')
    arrow.type = 'button'
    arrow.title = `repos of ${a.login}`
    arrow.setAttribute('aria-label', arrow.title)
    arrow.setAttribute('aria-expanded', String(open))
    arrow.dataset.pick = 'o:' + key
    arrow.addEventListener('click', () => toggleRepos(a.login))
    let count = ''
    if (p && p.all) count = 'all repos'
    else if (p) count = choice && choice.done ? `${p.repos.size} of ${choice.repos.length}` : plural(p.repos.size, 'repo')
    row.append(label, el('span', 'kind dim', a.kind), el('span', 'count ' + (p ? 'green' : 'dim'), count), arrow)
    box.append(row)
    if (open) box.append(repoList(a.login))
  }
  box.append(el('div', 'summary dim', pickSummary(selection())))
  const again = focused && [...box.querySelectorAll('[data-pick]')].find(e => e.dataset.pick === focused)
  if (!again) return
  again.focus()
  if (again.type === 'search') again.setSelectionRange(again.value.length, again.value.length)
}

function toggleRepos(owner) {
  const key = owner.toLowerCase()
  repoFilter = ''
  if (openOwner === key) openOwner = ''
  else {
    openOwner = key
    const choice = repoChoices.get(key)
    if (!choice || !choice.done || choice.days !== formDays()) return loadRepoChoices(owner)
  }
  renderAccounts()
}

async function loadRepoChoices(owner) {
  const key = owner.toLowerCase()
  const seq = ++repoSeq
  const days = formDays()
  const within = days > 0 ? ` pushed in the last ${days} ${days === 1 ? 'day' : 'days'}` : ''
  const choice = { days, repos: [], done: false, note: `loading repositories of ${owner}${within}…`, cls: 'dim' }
  repoChoices.set(key, choice)
  renderAccounts()
  const cutoff = Date.now() - days * 24 * 3600 * 1000
  const hit = cachedRepos(owner, days)
  const repos = hit ? [...hit] : []
  let failure = ''
  if (!hit) try {
    let next = await reposURL(owner, async url => (await connectGet(url)).data)
    while (next) {
      const page = await connectGet(next)
      if (seq !== repoSeq) return
      let older = false
      for (const r of page.data) {
        if (r.archived) continue
        if (days > 0 && !r.pushed_at) continue
        if (days > 0 && Date.parse(r.pushed_at) <= cutoff) {
          older = true
          break
        }
        repos.push({ name: r.name, pushed: r.pushed_at ? Date.parse(r.pushed_at) : 0 })
      }
      choice.note = `loading repositories of ${owner}${within}… ${repos.length}`
      renderAccounts()
      next = older ? '' : page.next
    }
  } catch (err) {
    if (seq !== repoSeq) return
    failure = err.message
  }
  if (!failure && !hit) storeRepos(owner, days, repos)
  choice.repos = repos
  choice.done = !failure
  if (failure) {
    choice.cls = 'red'
    choice.note = `cannot list repositories of ${owner}: ${failure}`
  } else if (!repos.length) {
    choice.cls = 'yellow'
    choice.note = days > 0
      ? `no repositories${within}; raise "Pushed within" or set it to 0`
      : 'this token sees no repositories here; check the token\'s resource owner'
  } else {
    choice.cls = 'dim'
    choice.note = `${repos.length} ${repos.length === 1 ? 'repository' : 'repositories'}${within}`
  }
  renderAccounts()
}

async function addOwner() {
  let name = $('org').value.trim()
  if (!name) return $('org').focus()
  if (!/^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/.test(name)) {
    $('loginError').textContent = 'not a GitHub user or organisation name'
    return $('org').focus()
  }
  $('loginError').textContent = ''
  const known = pickRows().find(a => a.login.toLowerCase() === name.toLowerCase())
  if (known) name = known.login
  else {
    setBusy('checking owner…')
    try {
      const { data } = await connectGet(`${API}/users/${encodeURIComponent(name)}`)
      name = data.login
      accounts.push({ login: name, kind: data.type === 'Organization' ? 'org' : 'user' })
    } catch (err) {
      $('loginError').textContent = `cannot find ${name}: ${err.message}`
      return $('org').focus()
    } finally {
      setBusy('')
    }
  }
  const key = name.toLowerCase()
  if (!picks.has(key)) picks.set(key, { owner: name, all: true, repos: new Set() })
  $('org').value = ''
  renderAccounts()
}

function targetLabel(targets) {
  return targets.map(t => t.all ? t.owner : t.repos.length === 1 ? `${t.owner}/${t.repos[0]}` : `${t.owner} (${t.repos.length} repos)`).join(', ')
}

function repoName(j) {
  return state.targets.length > 1 ? `${j.owner}/${j.repo}` : j.repo
}

function filterJobs(jobs, needle) {
  if (!needle) return jobs
  const n = needle.toLowerCase()
  return jobs.filter(j => [repoName(j), j.workflow, j.branch, j.name, j.runner, j.labels].some(f => f.toLowerCase().includes(n)))
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
  running.forEach(j => add(repoName(j), 'running'))
  queued.forEach(j => add(repoName(j), 'queued'))
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

function zoneLabel(d) {
  const off = -d.getTimezoneOffset()
  if (!off) return 'UTC'
  const abs = Math.abs(off)
  return `UTC${off < 0 ? '-' : '+'}${Math.floor(abs / 60)}${abs % 60 ? ':' + String(abs % 60).padStart(2, '0') : ''}`
}

function clock(ms, seconds) {
  const d = new Date(ms)
  const parts = seconds ? [d.getHours(), d.getMinutes(), d.getSeconds()] : [d.getHours(), d.getMinutes()]
  return `${parts.map(n => String(n).padStart(2, '0')).join(':')} ${zoneLabel(d)}`
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
  if (!state.targets.length) {
    s.append(el('div', 'dim', 'not connected'))
    return renderInfo()
  }
  const owners = el('div', 'line cyan bold', targetLabel(state.targets))
  owners.title = targetLabel(state.targets)
  const first = state.stale
    ? el('div', 'line yellow', `last results from ${state.lastScan}`)
    : el('div', 'line dim', `${state.repoCount >= 0 ? state.repoCount + ' repos' : '…'} · scanned ${state.lastScan || '-'}`)
  let second
  if (state.scanning && state.progress.total < 0) second = el('div', 'line dim', 'listing repos…')
  else if (state.scanning) second = el('div', 'line dim', `scanning ${state.progress.done}/${state.progress.total} repos`)
  else if (state.unseen > 0) second = el('div', 'line yellow', `first pass · ${state.unseen} ${state.unseen === 1 ? 'repo' : 'repos'} not checked yet`)
  else if (state.cost) second = el('div', 'line dim', `last scan ${state.cost.requests} req, ${state.cost.free} free (304)`)
  else second = el('div', 'line dim', 'no scan yet')
  s.append(owners, first, second)
  if (state.error) s.append(el('div', 'red', state.error))
  const failed = state.cost ? Object.entries(state.cost.failed) : []
  if (failed.length) {
    const counts = failed.map(([status, n]) => `${status} ×${n}`).join(', ')
    s.append(el('div', 'red', `${failed.reduce((t, [, n]) => t + n, 0)} requests refused (${counts}): ${state.cost.message}`))
  }
  if (!state.scanning && state.cost && state.progress.total === 0)
    s.append(el('div', 'yellow', `this token sees no repos in ${targetLabel(state.targets)} pushed in the last ${state.days} days; check the token's resource owner`))
  renderInfo()
}

function renderInfo() {
  const info = $('info')
  info.replaceChildren()
  const u = state.usage
  if (!state.targets.length) return
  const parts = [el('span', 'cyan bold', targetLabel(state.targets)), el('span', '', ` · next ${state.scanning ? '…' : state.nextIn + 's'}`)]
  if (state.pending) parts.push(el('span', '', ' · '), el('span', 'yellow', `next scan: ${targetLabel(state.pending.targets)}`))
  if (u.limit) {
    const cls = u.remaining < u.limit * 0.1 ? 'red' : u.remaining < u.limit * 0.3 ? 'yellow' : 'green'
    parts.push(el('span', '', ' · '), el('span', cls, `api ${u.remaining}/${u.limit}`), el('span', '', ` · resets ${clock(u.reset * 1000)}`))
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
  renderTable('runTable', 0, [['Runner', '24%'], ['Repo', '24%'], ['Job', '30%'], ['Step', '9%'], ['Elapsed', '13%']], j => [
    [j.runner, 'green'], [repoName(j), ''], [j.name, ''], [stepFraction(j.step), ''], [j.startedAt ? fmtElapsed(now - j.startedAt) : '-', ''],
  ])
  renderTable('qTable', 1, [['Wait', '12%'], ['Repo', '26%'], ['Job', '32%'], ['Labels', '30%']], j => {
    const mins = j.createdAt ? Math.floor((now - j.createdAt) / 60000) : -1
    return [[mins >= 0 ? mins + 'm' : '-', waitClass(mins)], [repoName(j), ''], [j.name, ''], [j.labels, '']]
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
  box.append(el('div', 'title', `${repoName(j)} › ${j.workflow} › ${j.name}`))
  const rows = [['Status', j.status === 'in_progress' ? 'running' : j.status, j.status === 'in_progress' ? 'green bold' : 'yellow bold']]
  if (j.status === 'in_progress') {
    rows.push(['Runner', j.runner || '-', 'green'])
    if (j.step && j.step.total) rows.push(['Step', `${stepFraction(j.step)} · ${j.step.name}`])
    rows.push(['Elapsed', j.startedAt ? `${fmtElapsed(now - j.startedAt)}  (since ${clock(j.startedAt)})` : '-'])
  } else {
    const mins = j.createdAt ? Math.floor((now - j.createdAt) / 60000) : -1
    rows.push(['Waiting', mins >= 0 ? `${mins}m  (since ${clock(j.createdAt)})` : '-', waitClass(mins)])
  }
  rows.push(['Branch', j.branch], ['Labels', j.labels.split(',').join(', ')], ['Run', '#' + j.runId], ['URL', safeURL(j.url).replace('https://', ''), 'blue'])
  kv(box, rows)
  if (j.status !== 'in_progress') {
    const busy = busyRunners(j, state.running)
    const wrap = el('div', 'busy')
    wrap.append(el('div', 'dim', busy.length ? 'Busy runners with these labels' : 'No running job shares these labels: runners may be offline'))
    for (const r of busy) {
      const row = el('div', 'row')
      row.append(el('span', 'green', r.runner), el('span', '', `${repoName(r)} › ${r.name}`), el('span', '', r.startedAt ? fmtElapsed(now - r.startedAt) : '-'))
      wrap.append(row)
    }
    box.append(wrap)
  }
}

function renderHints() {
  const hints = $('hints')
  hints.replaceChildren()
  const open = !$('login').hidden
  $('configBtn').classList.toggle('on', open)
  $('configBtn').textContent = open ? '✕ close config' : '⚙ config'
  $('configBtn').disabled = open && !token
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
  if (!token || state.scanning || !$('login').hidden) return
  state.nextIn--
  if (state.nextIn <= 0) return startScan()
  renderInfo()
  renderLists()
  renderDetail()
}

function submitConnect(e) {
  e.preventDefault()
  if ($('submit').disabled) return
  if (!$('stepUnlock').hidden) return submitUnlock()
  if (!$('stepToken').hidden) return submitToken()
  if ($('org').value.trim()) return addOwner()
  if (!selection().length) {
    $('loginError').textContent = 'tick at least one account or repository'
    return ($('accounts').querySelector('input') || $('org')).focus()
  }
  $('loginError').textContent = ''
  start()
}

function sameView(a, b) {
  return a.days === b.days && JSON.stringify(a.targets) === JSON.stringify(b.targets)
}

function viewKey() {
  return JSON.stringify(state.targets) + '|' + state.days
}

function applyPending() {
  const p = state.pending
  state.pending = null
  state.targets = p.targets
  state.days = p.days
  state.jobs = []
  state.stale = false
  state.repoCount = -1
  state.cost = null
  state.lastScan = ''
  state.error = ''
  state.progress = { done: 0, total: -1 }
  refresh()
}

async function start() {
  const targets = selection()
  if (!targets.length || !token) return
  const pick = { targets, days: formDays() }
  state.interval = Math.max(MIN_INTERVAL, parseInt($('interval').value, 10) || INTERVAL)
  const first = !state.targets.length
  state.pending = first || !sameView(pick, state) ? pick : null
  saveSettings()
  if (hasSession()) await keepSession(token, currentView())
  sealVault(token, currentView()).catch(() => {})
  await saveView(currentView())
  closeConnect()
  refresh()
  if (!first) return
  state.scanId++
  state.scanning = false
  state.nextIn = state.interval
  state.usage = { limit: 0, remaining: 0, reset: 0 }
  applyPending()
  if (lastResults && lastResults.key === viewKey()) {
    for (const j of lastResults.jobs) repoHints.add(`${j.owner}/${j.repo}`.toLowerCase())
    state.jobs = lastResults.jobs
    state.lastScan = clock(lastResults.at, true)
    state.stale = true
    document.title = windowTitle(state.jobs)
    refresh()
  }
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
    localStorage.setItem(SETTINGS_KEY, JSON.stringify({ days: (state.pending || state).days, interval: state.interval }))
  } catch {}
}

function init() {
  if (window.top !== window.self) {
    document.body.replaceChildren(el('p', 'red', 'gh-runner refuses to run inside a frame.'))
    return
  }
  loadSettings()
  renderHints()
  $('login').addEventListener('submit', submitConnect)
  $('changeToken').addEventListener('click', () => {
    dropSession()
    showTokenStep()
  })
  $('forgetToken').addEventListener('click', () => {
    forgetToken()
    dropSession()
    $('loginError').textContent = ''
    showTokenStep()
  })
  $('suggestPass').addEventListener('click', suggestPassphrase)
  $('configBtn').addEventListener('click', toggleConnect)
  $('remember').addEventListener('change', () => {
    $('rememberFields').hidden = !$('remember').checked
    if ($('remember').checked) $('newPass').focus()
  })
  $('addOwner').addEventListener('click', addOwner)
  $('days').addEventListener('input', () => {
    clearTimeout(daysTimer)
    daysTimer = setTimeout(() => {
      const owner = pickRows().find(a => a.login.toLowerCase() === openOwner)
      if (owner) loadRepoChoices(owner.login)
    }, 500)
  })
  $('filter').addEventListener('input', () => {
    state.filter = $('filter').value
    refresh()
  })
  document.addEventListener('keydown', onKey)
  setInterval(tick, 1000)
  refresh()
  resumeSession()
}

init()
