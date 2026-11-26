// The chat store: module-scope on purpose. A tab switch unmounts Chat.vue
// but must NOT kill a running stream (the same bug dossier fixed after a
// two-minute research turn died on navigation) — so the state and the
// in-flight fetch live here, and the view re-attaches on mount.
//
// Ported from the router dashboard's chat.ts: the chats still live only
// in this browser (lib/chatStorage.js), but turns now go to the LOCAL
// OpenAI wire (POST /v1/chat/completions, stream: true) — direct to a
// node when a direct path exists, through the router otherwise. The
// router's web-search frames ride the same stream; there are no accounts
// and no node pinning here.
import { reactive, watch } from 'vue'
import { api, clientState, needsToken, storedToken, type ToolEvent } from './api'
import {
  emptyIndex, newMeta, deriveTitle, touch, removeChat, evict, oldestFirst,
  slimForStorage,
  type ChatIndex, type ChatMessage, type ChatMeta, type SearchSource, type ToolStep,
} from './lib/chats.js'
import { makeStorage } from './lib/chatStorage.js'

export type { ChatMessage, ChatMeta, SearchSource, ToolStep } from './lib/chats.js'

interface ChatState {
  index: ChatIndex
  activeId: string
  messages: ChatMessage[] // the active chat's messages
  streaming: boolean
  streamingId: string // the chat the in-flight turn belongs to ('' = none)
  error: string // the active chat's turn-level error (402, network, offline model…)
  model: string // mirrors the active chat ('' = auto-pick on first send)
  webSearch: boolean // pre-generation web search on the next turns
  effort: string // thinking effort: '' = model default, else off|low|medium|high
  loaded: boolean
  storageOK: boolean
  notice: string // storage trouble worth one line in the list
}

const state = reactive<ChatState>({
  index: emptyIndex(),
  activeId: '',
  messages: [],
  streaming: false,
  streamingId: '',
  error: '',
  model: '',
  webSearch: false,
  effort: '',
  loaded: false,
  storageOK: true,
  notice: '',
})

const storage = makeStorage()
state.storageOK = storage.ok

let abort: AbortController | null = null
// Loaded chats' message lists (reactive), so the streaming chat keeps one
// array across switches and coming back shows images not yet evicted.
const cache = new Map<string, ChatMessage[]>()

const MAX_TURNS = 40 // keep the request history bounded (storage keeps more)

// UI_NOTES are what the page writes into an assistant turn that produced no
// answer. They are notes for the reader, never model output: the history a
// turn sends must not carry them (a model reading "(stopped before the first
// token)" as its own words talks about an error that never happened to it).
const NOTE_LENGTH = '(the model ran out of output tokens while still thinking — no answer was written; send "continue" to have it finish)'
const NOTE_NO_ANSWER = '(the model thought but wrote no answer — send "continue" to have it answer)'
const NOTE_STOPPED = '(stopped before the first token)'
const NOTE_EMPTY = '(empty response)'
const UI_NOTES = new Set([NOTE_LENGTH, NOTE_NO_ANSWER, NOTE_STOPPED, NOTE_EMPTY])
const STREAM_SAVE_MS = 2000 // a closed tab keeps most of a long answer

function meta(id: string): ChatMeta | undefined {
  return state.index.chats.find((c) => c.id === id)
}

export function activeMeta(): ChatMeta | undefined {
  return meta(state.activeId)
}

// evictOne frees storage for a quota-failed write: the oldest chat that is
// neither on screen nor streaming.
function evictOne(): boolean {
  const id = oldestFirst(state.index, [state.activeId])[0]
  if (!id) return false
  dropChat(id)
  state.notice = 'Browser storage is full — the oldest chats were removed.'
  return true
}

function dropChat(id: string) {
  state.index.chats = state.index.chats.filter((c) => c.id !== id)
  cache.delete(id)
  storage.deleteMessages(id)
}

// saveIndex stores chats that have messages; an empty "New chat" lives in
// memory until its first message, so clicking New chat leaves no litter.
function saveIndex() {
  if (!state.loaded) return
  const idx: ChatIndex = {
    v: 1,
    active: state.activeId,
    chats: state.index.chats.filter((c) => c.count > 0).map((c) => ({ ...c })),
  }
  storage.saveIndex(idx, evictOne)
}

function saveMessages(id: string) {
  const list = cache.get(id)
  const m = meta(id)
  if (!state.loaded || !list || !m || m.count === 0) return
  storage.saveMessages(id, slimForStorage(list), evictOne)
}

let streamSaveTimer: ReturnType<typeof setTimeout> | null = null
function saveStreamingSoon(id: string) {
  if (streamSaveTimer) return
  streamSaveTimer = setTimeout(() => {
    streamSaveTimer = null
    saveMessages(id)
  }, STREAM_SAVE_MS)
}

// syncURL keeps ?chat=<id> so a reload returns to the same chat (only
// while the Chat tab is open; App.vue owns the other keys).
function syncURL() {
  try {
    const u = new URL(window.location.href)
    if (u.searchParams.get('tab') !== 'chat') return
    const m = activeMeta()
    if (m && m.count > 0) u.searchParams.set('chat', m.id)
    else u.searchParams.delete('chat')
    window.history.replaceState(null, '', u)
  } catch { /* no window (tests) */ }
}

// show puts a chat on screen: messages, then its settings.
function show(id: string) {
  const m = meta(id)
  if (!m) return
  let list = cache.get(id)
  if (!list) {
    state.messages = storage.loadMessages(id)
    list = state.messages
    cache.set(id, list)
  } else {
    state.messages = list
  }
  state.activeId = id
  state.index.active = id
  state.error = ''
  state.model = m.model
  state.effort = m.effort
  if (m.webSearch !== null) state.webSearch = m.webSearch
  syncURL()
}

function startFresh(): ChatMeta {
  const m = newMeta(Date.now(), { model: state.model, effort: state.effort })
  state.index.chats.unshift(m)
  cache.set(m.id, [])
  show(m.id)
  return m
}

// restoreChats loads the browser's chats once at boot.
export function restoreChats() {
  if (state.loaded) return
  state.loaded = true
  const idx = storage.loadIndex() ?? emptyIndex()
  idx.chats.sort((a, b) => b.updated - a.updated)
  state.index = idx
  let want = ''
  try { want = new URLSearchParams(window.location.search).get('chat') ?? '' } catch { /* no window */ }
  const first = meta(want)?.id ?? meta(idx.active)?.id ?? idx.chats[0]?.id
  if (first) show(first)
  else startFresh()
}

// newChat opens an empty chat; while the active one is still empty it is
// reused (one empty chat at most). Allowed while another chat streams.
export function newChat() {
  const m = activeMeta()
  if (m && m.count === 0) return
  startFresh()
}

export function switchChat(id: string) {
  if (id === state.activeId || !meta(id)) return
  // Leaving an empty chat drops it: it was never stored.
  const leaving = activeMeta()
  if (leaving && leaving.count === 0) {
    state.index.chats = state.index.chats.filter((c) => c.id !== leaving.id)
    cache.delete(leaving.id)
  }
  show(id)
  saveIndex()
}

export function renameChat(id: string, title: string) {
  const m = meta(id)
  const t = title.trim().replace(/\s+/g, ' ').slice(0, 120)
  if (!m || !t) return
  m.title = t
  m.titleSet = true
  saveIndex()
}

export function deleteChat(id: string) {
  if (state.streamingId === id) stopStreaming()
  const wasActive = state.activeId === id
  state.index.active = state.activeId
  const next = removeChat(state.index, id)
  cache.delete(id)
  storage.deleteMessages(id)
  if (wasActive) {
    if (next) show(next)
    else startFresh()
  }
  saveIndex()
}

export function deleteAllChats() {
  if (abort) abort.abort()
  for (const c of state.index.chats) storage.deleteMessages(c.id)
  state.index.chats = []
  cache.clear()
  state.streamingId = ''
  startFresh()
  saveIndex()
}

// openInChat is a ?model= link: a chat with history keeps it — the model
// opens in a fresh chat; an empty chat is reused.
export function openInChat(modelID: string) {
  const m = activeMeta()
  if (m && m.count > 0 && modelID && modelID !== m.model) startFresh()
  if (modelID) state.model = modelID
}

// setWebSearch is the user's toggle: from now on this chat keeps its own
// choice instead of following the model's default.
export function setWebSearch(on: boolean) {
  state.webSearch = on
  const m = activeMeta()
  if (m) {
    m.webSearch = on
    saveIndex()
  }
}

// The toolbar writes model / effort into the active chat. After a switch
// the values equal the new chat's, so this writes nothing new.
watch(() => [state.model, state.effort] as const, ([model, effort]) => {
  const m = activeMeta()
  if (!m || (m.model === model && m.effort === effort)) return
  m.model = model
  m.effort = effort
  saveIndex()
})

export function stopStreaming() {
  if (abort) abort.abort()
}

// useChatState hands the view the module-scope store.
export function useChatState(): ChatState {
  return state
}

// pickModel resolves '' to the first live chat model at send time. The
// local proxy forwards /api/v1/models to the router.
async function pickModel(): Promise<string> {
  if (state.model) return state.model
  const models = (await api.models()).models
  const chat = models.filter((m) => m.modality !== 'embedding' && m.specialty !== 'ocr')
  const live = chat.filter((m) => m.live_nodes > 0)
  const pool = live.length ? live : chat
  const feat = pool.find((m) => m.featured)
  const chosen = (feat ?? pool[0])?.id ?? ''
  if (!chosen) throw new Error('no models on the network yet — wait for a node to connect')
  state.model = chosen
  return chosen
}

// sendChat runs one turn: POST /v1/chat/completions with the visible
// history, streaming the reply into a live assistant message. The OpenAI
// chunk wire is parsed by hand — frame boundaries do not align with
// network chunks, and `data: [DONE]` is not JSON.
export async function sendChat(text: string, images: string[] = []) {
  if (state.streaming) return
  const content = text.trim()
  if (!content && images.length === 0) return
  state.error = ''

  let model: string
  try {
    model = await pickModel()
  } catch (e) {
    state.error = String(e).replace(/^Error: /, '')
    return
  }

  // The turn belongs to this chat for its whole life, whichever chat the
  // user looks at meanwhile.
  const chatId = state.activeId
  const chat = meta(chatId)
  const msgs = state.messages
  if (!chat) return
  msgs.push({ role: 'user', text: content, images: images.length ? images : undefined })
  if (chat.count === 0 && !chat.titleSet) {
    chat.title = deriveTitle(content, images.length > 0, [])
  }
  msgs.push({ role: 'assistant', text: '', model })
  // The reactive wrapper, not the raw object: mutating the raw target
  // changes the value behind Vue's proxy without triggering anything, so
  // the view would only repaint when the turn ends.
  const shell = msgs[msgs.length - 1]
  chat.count = msgs.length
  touch(state.index, chatId, Date.now())
  for (const id of evict(state.index, [chatId, state.activeId])) dropChat(id)
  state.streaming = true
  state.streamingId = chatId
  saveIndex()
  saveMessages(chatId)
  syncURL()

  abort = new AbortController()
  try {
    // Multimodal turns speak content parts; text-only stays a plain
    // string so non-vision backends see exactly what they always saw.
    const parts = (m: ChatMessage) => {
      if (!m.images?.length) return m.text
      return [
        ...(m.text ? [{ type: 'text', text: m.text }] : []),
        ...m.images.map((url) => ({ type: 'image_url', image_url: { url } })),
      ]
    }
    const history = msgs
      .filter((m) => !m.error && !(m.role === 'assistant' && UI_NOTES.has(m.text)) && (m.text || m.images?.length || m.role === 'user'))
      .map((m) => ({ role: m.role, content: m.role === 'assistant' ? m.text || '(empty)' : parts(m) }))
      .slice(-MAX_TURNS)

    if (clientState.value.toolsEnabled) {
      // The tool loop: the server runs the rounds; the page renders the
      // events (tool calls, permission prompts, results) as they come.
      const resp = await fetch('/ui/api/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...tokenHeaders() },
        body: JSON.stringify({
          model, messages: history, stream: true,
          effort: state.effort || undefined,
          web_search: state.webSearch || undefined,
        }),
        signal: abort.signal,
      })
      if (resp.status === 401) needsToken.value = true
      if (!resp.ok || !resp.body) {
        let msg = `HTTP ${resp.status}`
        try {
          const errBody = await resp.json()
          msg = errBody?.error?.message ?? msg
        } catch { /* not JSON */ }
        throw new Error(msg)
      }
      const finish = await readToolStream(resp.body, shell, chatId)
      // The same silent endings the plain stream handles: a turn that
      // produced only thinking (or nothing) and stopped says so instead
      // of vanishing under the cursor.
      if (!shell.text && !shell.reasoning && finish && finish !== 'tool_calls') {
        shell.text =
          finish === 'length'
            ? NOTE_LENGTH
            : NOTE_NO_ANSWER
      }
    } else {
      const resp = await plainChatBody(model, history)
      shell.path = resp.headers.get('X-Remote-Path') || undefined
      const finish = await readPlainStream(resp, shell, chatId)
      applyPlainEnding(shell, finish)
    }
    shell.model = model
  } catch (e) {
    const err = e as Error
    if (err.name === 'AbortError') {
      if (!shell.text && !shell.reasoning) shell.text = NOTE_STOPPED
    } else {
      const msg = String(err?.message ?? e).replace(/^Error: /, '')
      if (state.activeId === chatId) state.error = msg
      shell.error = msg
    }
  } finally {
    state.streaming = false
    state.streamingId = ''
    abort = null
    if (streamSaveTimer) {
      clearTimeout(streamSaveTimer)
      streamSaveTimer = null
    }
    chat.count = msgs.length
    chat.updated = Date.now()
    saveIndex()
    saveMessages(chatId)
  }
}

function tokenHeaders(): Record<string, string> {
  const tok = storedToken()
  return tok ? { 'X-Local-Token': tok } : {}
}

// ── the plain turn: the local OpenAI wire ────────────────────────────

async function plainChatBody(model: string, history: unknown[]): Promise<Response> {
  const body: Record<string, unknown> = {
    model, messages: history, stream: true,
    reasoning_effort: state.effort || undefined,
  }
  if (state.webSearch) body.web_search = true
  const resp = await fetch('/v1/chat/completions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...tokenHeaders() },
    body: JSON.stringify(body),
    signal: abort!.signal,
  })
  if (resp.status === 401) needsToken.value = true
  if (!resp.ok || !resp.body) {
    let msg = `HTTP ${resp.status}`
    try {
      const errBody = await resp.json()
      msg = errBody?.error?.message ?? msg
    } catch { /* not JSON */ }
    throw new Error(msg)
  }
  return resp
}

// applyPlainEnding is the plain stream's silent-end handling: a stream
// that completed with thinking but no answer text used to end in
// silence — the cursor vanishes, nothing replaces it, no error card.
// The wire told the truth; the UI says what happened.
function applyPlainEnding(shell: ChatMessage, finish: string | null) {
  if (!shell.text && !shell.reasoning && finish) {
    shell.text =
      finish === 'length'
        ? NOTE_LENGTH
        : NOTE_NO_ANSWER
  }
}

// readPlainStream parses the OpenAI chunk wire by hand — frame boundaries
// do not align with network chunks, and `data: [DONE]` is not JSON —
// streaming the reply into the live assistant message. Returns the
// finish reason (null when the stream never said).
async function readPlainStream(resp: Response, shell: ChatMessage, chatId: string): Promise<string | null> {
  const reader = resp.body!.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let usage: { input: number; output: number } | null = null
  let finish: string | null = null
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let split: number
    while ((split = buffer.indexOf('\n\n')) !== -1) {
      const frame = buffer.slice(0, split)
      buffer = buffer.slice(split + 2)
      for (const line of frame.split('\n')) {
        if (!line.startsWith('data:')) continue
        const payload = line.slice(5).trim()
        if (payload === '[DONE]') continue
        let chunk: {
          choices?: Array<{ delta?: { content?: string; reasoning_content?: string }; finish_reason?: string | null }>
          usage?: { prompt_tokens: number; completion_tokens: number }
          web_search?: { query?: string; results?: SearchSource[]; note?: string }
          error?: { message?: string; code?: string }
        }
        try {
          chunk = JSON.parse(payload)
        } catch {
          continue // keepalive or unparseable frame
        }
        // A failure after the stream opened arrives as an error event —
        // the turn's error card, not a silent end.
        if (chunk.error) throw new Error(chunk.error.message ?? chunk.error.code ?? 'the stream failed')
        const choice = chunk.choices?.[0]
        if (choice?.finish_reason) finish = choice.finish_reason
        const delta = choice?.delta
        if (delta?.content) shell.text += delta.content
        if (delta?.reasoning_content) {
          shell.reasoning = (shell.reasoning ?? '') + delta.reasoning_content
        }
        if (delta?.content || delta?.reasoning_content) saveStreamingSoon(chatId)
        if (chunk.web_search) {
          // Sources frame: shown while the answer streams in. The router
          // emits one SNAPSHOT per executed search/fetch (the agentic
          // loop), so accumulate and dedupe by url.
          const ws = chunk.web_search
          if (ws.query) {
            shell.searchQuery = ws.query
            if (!shell.searchQueries?.includes(ws.query)) {
              shell.searchQueries = [...(shell.searchQueries ?? []), ws.query]
            }
          }
          const seen = new Set((shell.search ?? []).map((s) => s.url))
          shell.search = [
            ...(shell.search ?? []),
            ...(ws.results ?? []).filter((r) => !seen.has(r.url)),
          ]
          if (ws.note) shell.searchNote = ws.note
        }
        if (chunk.usage) {
          usage = { input: chunk.usage.prompt_tokens, output: chunk.usage.completion_tokens }
        }
      }
    }
  }
  if (usage && !shell.text && !shell.reasoning) {
    shell.text = NOTE_EMPTY
  }
  return finish
}

// ── the tool turn: the agent loop's event stream ─────────────────────

// readToolStream parses POST /ui/api/chat's SSE (one agent.Event per
// frame): content deltas, tool calls, the permission prompts the user
// answers inline, results, and the per-round path. Returns the final
// round's finish reason.
async function readToolStream(body: ReadableStream<Uint8Array>, shell: ChatMessage, chatId: string): Promise<string | null> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let finish: string | null = null
  const ensureTool = (id: string): ToolStep => {
    shell.tools = shell.tools ?? []
    let t = shell.tools.find((x) => x.id === id)
    if (!t) {
      t = { id, name: '', server: '', tool: '', arguments: '' }
      shell.tools.push(t)
    }
    return t
  }
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let split: number
    while ((split = buffer.indexOf('\n\n')) !== -1) {
      const frame = buffer.slice(0, split)
      buffer = buffer.slice(split + 2)
      for (const line of frame.split('\n')) {
        if (!line.startsWith('data:')) continue
        const payload = line.slice(5).trim()
        if (payload === '[DONE]') continue
        let ev: ToolEvent
        try {
          ev = JSON.parse(payload)
        } catch {
          continue // keepalive or unparseable frame
        }
        switch (ev.type) {
          case 'delta':
            if (ev.content) shell.text += ev.content
            if (ev.reasoning) shell.reasoning = (shell.reasoning ?? '') + ev.reasoning
            if (ev.content || ev.reasoning) saveStreamingSoon(chatId)
            break
          case 'tool_call': {
            const t = ensureTool(ev.id ?? String(shell.tools?.length ?? 0))
            t.name = ev.name ?? t.name
            t.server = ev.server ?? t.server
            t.tool = ev.tool ?? t.tool
            t.arguments = ev.arguments ?? t.arguments
            break
          }
          case 'permission': {
            // The ask rides the tool entry the call created (matched by
            // server+tool; the loop asks exactly one tool at a time).
            const pending = (shell.tools ?? []).filter((t) => t.server === ev.server && t.tool === ev.tool && !t.done)
            const t = pending.length ? pending[pending.length - 1] : ensureTool(ev.id ?? 'ask')
            t.askId = ev.id
            break
          }
          case 'tool_result': {
            const t = ensureTool(ev.id ?? String(shell.tools?.length ?? 0))
            t.done = true
            t.ok = ev.ok ?? false
            t.denied = ev.text === 'denied by the user'
            t.ms = ev.ms
            t.bytes = ev.bytes
            t.result = ev.text
            break
          }
          case 'disclosure':
            // The privacy boundary, once per server: local file contents
            // leave the machine to wherever the model is served.
            shell.disclosure = ev.text ?? shell.disclosure
            break
          case 'done':
            shell.path = ev.path || shell.path
            if (ev.finish) finish = ev.finish
            break
          case 'error':
            throw new Error(ev.message ?? 'the turn failed')
        }
      }
    }
  }
  return finish
}

// answerPermission is the inline prompt's button: the decision goes to
// /ui/api/tools/decide and the loop continues on the same stream.
export async function answerPermission(shell: ChatMessage, decision: 'once' | 'always' | 'deny') {
  const pending = (shell.tools ?? []).find((t) => t.askId && !t.answered)
  if (!pending || !pending.askId) return
  pending.answered = decision
  await api.decide(pending.askId, decision)
}
