// The client UI's fetch layer. Everything it talks to is the LOCAL
// endpoint: /ui/api/* (settings, key, doctor — the client's own surface)
// and /v1 + /api/v1/models (the proxy: OpenAI wire, models forwarded to
// the router). The API key itself never reaches this page — the proxy
// adds it server-side.
//
// Two headers matter:
//  - X-RC-CSRF on every state-changing /ui/api call: the token the serve
//    process baked into this page's <meta name="rc-csrf">. Missing or
//    wrong answers 403.
//  - X-Local-Token on every call when the listener is not loopback-only:
//    the token the user pasted once, kept in sessionStorage (a page
//    reload drops it by design — session-scoped like the browser tab).
import { ref } from 'vue'

export interface ModelCard {
  id: string; display_name: string; modality: string; specialty: string
  live_nodes: number; featured: boolean
  vision: boolean; tools: boolean; reasoning: boolean
  reasoning_efforts?: string[]
}

export interface SettingView {
  key: string
  label: string
  value: string | boolean | null
  set: boolean
  source: string // flag|env|file|default|unset
  source_name: string // the flag or env var's name
  locked: boolean
  lock_reason: string
  kind: 'string' | 'bool' | 'secret' | 'info'
  secret: boolean
  note?: string
}

const TOKEN_KEY = 'rc_local_token'

export function storedToken(): string {
  try { return sessionStorage.getItem(TOKEN_KEY) ?? '' } catch { return '' }
}

export function saveToken(tok: string) {
  try { sessionStorage.setItem(TOKEN_KEY, tok) } catch { /* blocked */ }
}

// needsToken flips on when the endpoint answers 401 — App.vue shows the
// token dialog, and every in-flight surface re-runs after it closes.
export const needsToken = ref(false)

function csrfToken(): string {
  const el = document.querySelector('meta[name="rc-csrf"]')
  const v = el?.getAttribute('content') ?? ''
  return v && v !== '__RC_CSRF__' ? v : ''
}

async function parseError(r: Response): Promise<string> {
  try {
    const j = await r.json()
    if (j?.error?.message) return String(j.error.message)
    if (typeof j?.error === 'string') return j.error
  } catch { /* not JSON */ }
  return `HTTP ${r.status}`
}

export async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { headers: tokenHeaders() })
  if (r.status === 401) { needsToken.value = true }
  if (!r.ok) throw new Error(await parseError(r))
  return r.json() as Promise<T>
}

// send is the state-changing shape: the CSRF header is required on every
// POST/PUT/DELETE to /ui/api (the proxy answers 403 without it).
export async function send<T>(path: string, method: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { ...tokenHeaders() }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const csrf = csrfToken()
  if (csrf) headers['X-RC-CSRF'] = csrf
  const r = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (r.status === 401) { needsToken.value = true }
  if (!r.ok) throw new Error(await parseError(r))
  return r.json() as Promise<T>
}

function tokenHeaders(): Record<string, string> {
  const tok = storedToken()
  return tok ? { 'X-Local-Token': tok } : {}
}

export interface ToolView {
  name: string
  server: string
  tool: string
  description: string
  mode: 'allow' | 'ask' | 'deny'
}

export interface ServerView {
  name: string
  tools: ToolView[]
}

// The agent loop's SSE event (POST /ui/api/chat), mirrored from the Go
// agent.Event. Tool arguments and results are rendered as text only.
export interface ToolEvent {
  type: 'round' | 'delta' | 'tool_call' | 'permission' | 'tool_result' | 'disclosure' | 'done' | 'error'
  round?: number
  content?: string
  reasoning?: string
  id?: string
  name?: string
  server?: string
  tool?: string
  arguments?: string
  ok?: boolean
  ms?: number
  bytes?: number
  text?: string
  path?: 'direct' | 'router' | ''
  model?: string
  finish?: string
  message?: string
}

export const api = {
  models: () => get<{ models: ModelCard[] }>('/api/v1/models'),
  settings: () => get<{ settings: SettingView[]; tools_enabled: boolean }>('/ui/api/settings'),
  setSetting: (key: string, value: string | boolean) =>
    send<{ ok: boolean; restart: boolean }>('/ui/api/settings', 'POST', { key, value }),
  replaceKey: (apiKey: string) => send<{ ok: boolean }>('/ui/api/login', 'POST', { api_key: apiKey }),
  doctor: () => send<{ ok: boolean; report: string }>('/ui/api/doctor', 'POST'),
  tools: () => get<{ servers: ServerView[]; disclosure: string; error?: string }>('/ui/api/tools'),
  setToolMode: (name: string, mode: ToolView['mode']) =>
    send<{ ok: boolean }>('/ui/api/tools/mode', 'POST', { name, mode }),
  decide: (ask_id: string, decision: 'once' | 'always' | 'deny') =>
    send<{ ok: boolean }>('/ui/api/tools/decide', 'POST', { ask_id, decision }),
}

// The one fetch every view shares at boot: whether turns run the tool
// loop (tools on AND at least one server configured), and the resolved
// web-search default.
export const clientState = ref<{ toolsEnabled: boolean; webSearchDefault: boolean }>({
  toolsEnabled: false,
  webSearchDefault: false,
})

export async function loadClientState(): Promise<void> {
  try {
    const s = await api.settings()
    clientState.value.toolsEnabled = s.tools_enabled
    for (const row of s.settings) {
      if (row.key === 'web_search' && row.kind === 'bool') clientState.value.webSearchDefault = !!row.value
    }
  } catch {
    /* the settings page shows the error; chat works without tools */
  }
}
