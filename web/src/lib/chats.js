// Chat list logic: pure functions over the chat index — no Vue, no DOM,
// no storage — so they stay testable in isolation (the md.js pattern).

export const SCHEMA = 1
export const MAX_CHATS = 100 // chats kept per account in this browser
export const MAX_STORED = 200 // messages kept per chat (requests send fewer)
export const NEW_TITLE = 'New chat'

export function emptyIndex() {
  return { v: SCHEMA, active: '', chats: [] }
}

export function newID() {
  try {
    if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID()
  } catch { /* insecure context */ }
  return Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10)
}

// newMeta starts a chat; it inherits the model and effort the user is on
// (people keep using one model), never the search toggle.
export function newMeta(now, defaults = {}) {
  return {
    id: newID(),
    title: NEW_TITLE,
    titleSet: false,
    created: now,
    updated: now,
    model: defaults.model ?? '',
    effort: defaults.effort ?? '',
    webSearch: null,
    count: 0,
  }
}

// deriveTitle names a chat after its first user message: the first line,
// whitespace collapsed, cut at 60 chars on a word boundary. Never empty.
export function deriveTitle(text, hasImages, docNames) {
  const line = String(text ?? '').split('\n').map((l) => l.trim()).find((l) => l) ?? ''
  const flat = line.replace(/\s+/g, ' ')
  if (flat) {
    if (flat.length <= 60) return flat
    const cut = flat.slice(0, 60)
    const sp = cut.lastIndexOf(' ')
    return (sp >= 30 ? cut.slice(0, sp) : cut).replace(/[\s,.;:!?-]+$/, '') + '…'
  }
  if (docNames?.length) return 'Document: ' + docNames[0]
  if (hasImages) return 'Image'
  return NEW_TITLE
}

// touch marks a chat as just used: newest first.
export function touch(index, id, now) {
  const i = index.chats.findIndex((c) => c.id === id)
  if (i < 0) return
  const [c] = index.chats.splice(i, 1)
  c.updated = now
  index.chats.unshift(c)
}

// removeChat drops a chat and returns the id that should become active:
// the neighbour that took its place, else the one before, else '' (the
// caller starts a fresh chat).
export function removeChat(index, id) {
  const i = index.chats.findIndex((c) => c.id === id)
  if (i < 0) return index.active
  index.chats.splice(i, 1)
  if (index.active !== id) return index.active
  const next = index.chats[i] ?? index.chats[i - 1]
  return next ? next.id : ''
}

// evict returns the ids to drop when the index is over max: oldest
// updated first, never one in keep (the active and the streaming chat).
export function evict(index, keep = [], max = MAX_CHATS) {
  const over = index.chats.length - max
  if (over <= 0) return []
  return oldestFirst(index, keep).slice(0, over)
}

// oldestFirst lists evictable ids, least recently updated first.
export function oldestFirst(index, keep = []) {
  return index.chats
    .filter((c) => !keep.includes(c.id))
    .sort((a, b) => a.updated - b.updated)
    .map((c) => c.id)
}

// slimForStorage keeps the last MAX_STORED messages without image data
// (base64 turns blow the ~5 MB quota in a turn or two); how many images a
// turn carried survives so the UI can say they were not kept.
export function slimForStorage(messages, max = MAX_STORED) {
  return messages.slice(-max).map((m) => {
    if (!m.images?.length) return { ...m, images: undefined }
    return { ...m, images: undefined, imageCount: (m.imageCount ?? 0) + m.images.length }
  })
}

function startOfDay(ms) {
  const d = new Date(ms)
  d.setHours(0, 0, 0, 0)
  return d
}

function daysBefore(day, n) {
  const d = new Date(day)
  d.setDate(d.getDate() - n) // calendar days: DST-safe, unlike n*24h
  return d.getTime()
}

// groupByAge buckets chats by local calendar day of their last update.
export function groupByAge(chats, now) {
  const today = startOfDay(now)
  const bounds = [
    ['Today', today.getTime()],
    ['Yesterday', daysBefore(today, 1)],
    ['Previous 7 days', daysBefore(today, 7)],
    ['Previous 30 days', daysBefore(today, 30)],
    ['Older', -Infinity],
  ]
  const groups = bounds.map(([label]) => ({ label, chats: [] }))
  for (const c of chats) {
    const i = bounds.findIndex(([, from]) => c.updated >= from)
    groups[i].chats.push(c)
  }
  return groups.filter((g) => g.chats.length)
}

// migrateLegacy turns the single pre-list conversation into one chat, or
// returns null when there is nothing usable.
export function migrateLegacy(raw, now, defaults = {}) {
  let parsed
  try {
    parsed = typeof raw === 'string' ? JSON.parse(raw) : raw
  } catch {
    return null
  }
  if (!Array.isArray(parsed)) return null
  const messages = parsed.filter((m) => m && (m.role === 'user' || m.role === 'assistant') && typeof m.text === 'string')
  if (!messages.length) return null
  const meta = newMeta(now, defaults)
  const first = messages.find((m) => m.role === 'user')
  meta.title = deriveTitle(first?.text, false, first?.docs?.map((d) => d.name))
  meta.count = messages.length
  return { meta, messages }
}

// filterChats is the list's title filter: case-insensitive substring.
export function filterChats(chats, q) {
  const needle = String(q ?? '').trim().toLowerCase()
  if (!needle) return chats
  return chats.filter((c) => c.title.toLowerCase().includes(needle))
}
