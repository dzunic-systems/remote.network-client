// Chat storage: one localStorage key for the chat index and one per chat,
// so a write touches only the chat that changed. Every access is guarded
// — private windows and blocked site data degrade to an in-memory
// session, never an exception in the UI. Nothing here leaves the browser.
// (Ported from the router dashboard's chatStorage.js; the client has no
// accounts, so the namespace is a fixed "local" instead of an email.)

const indexKey = () => 'rc_chats_v1'
const chatKey = (id) => 'rc_chat_v1_' + id

function isQuota(e) {
  return !!e && (e.name === 'QuotaExceededError' || e.name === 'NS_ERROR_DOM_QUOTA_REACHED' ||
    e.code === 22 || e.code === 1014)
}

function defaultStorage() {
  try {
    const ls = globalThis.localStorage
    const probe = '__rc_probe__'
    ls.setItem(probe, '1')
    ls.removeItem(probe)
    return ls
  } catch {
    return null
  }
}

// makeStorage wraps a Storage-like object (tests pass a fake). With no
// usable storage, ok is false and every call is a harmless no-op.
export function makeStorage(ls = defaultStorage()) {
  const ok = ls != null

  function read(key) {
    if (!ok) return null
    try {
      const raw = ls.getItem(key)
      return raw == null ? null : JSON.parse(raw)
    } catch {
      return null
    }
  }

  // write retries a quota failure after each evict() call frees room;
  // evict returns false when nothing more can go. Returns whether the
  // value landed.
  function write(key, value, evict) {
    if (!ok) return false
    const raw = JSON.stringify(value)
    for (let i = 0; i <= 5; i++) {
      try {
        ls.setItem(key, raw)
        return true
      } catch (e) {
        if (!isQuota(e) || !evict || i === 5 || !evict()) return false
      }
    }
    return false
  }

  function remove(key) {
    if (!ok) return
    try { ls.removeItem(key) } catch { /* blocked */ }
  }

  return {
    ok,
    loadIndex: () => read(indexKey()),
    saveIndex: (index, evict) => write(indexKey(), index, evict),
    loadMessages: (id) => {
      const v = read(chatKey(id))
      return Array.isArray(v) ? v : []
    },
    saveMessages: (id, messages, evict) => write(chatKey(id), messages, evict),
    deleteMessages: (id) => remove(chatKey(id)),
  }
}
