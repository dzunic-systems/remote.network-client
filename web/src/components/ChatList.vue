<script setup lang="ts">
// The chat list: every conversation this account kept in this browser,
// grouped by age, filterable by title. Rename inline, delete with a
// confirmation. A sidebar on wide screens; a drawer on narrow ones
// (Chat.vue toggles the `open` class).
import { computed, nextTick, ref } from 'vue'
import { deleteAllChats, deleteChat, newChat, renameChat, switchChat, useChatState } from '../chat'
import { filterChats, groupByAge, type ChatMeta } from '../lib/chats.js'

const emit = defineEmits<{ picked: [] }>()
const chatState = useChatState()

const query = ref('')
const now = ref(Date.now())
const groups = computed(() => {
  void chatState.index.chats.length // re-group as chats come and go
  return groupByAge(filterChats(chatState.index.chats, query.value), now.value)
})
const flat = computed(() => groups.value.flatMap((g) => g.chats))

function pick(id: string) {
  now.value = Date.now()
  switchChat(id)
  emit('picked')
}

function startNew() {
  newChat()
  emit('picked')
}

// ── rename ───────────────────────────────────────────────────
const editing = ref('')
const draft = ref('')
const editInput = ref<HTMLInputElement[] | null>(null)
function startRename(c: ChatMeta) {
  menuFor.value = ''
  editing.value = c.id
  draft.value = c.title
  nextTick(() => {
    const el = editInput.value?.[0]
    el?.focus()
    el?.select()
  })
}
function commitRename() {
  if (editing.value) renameChat(editing.value, draft.value)
  editing.value = ''
}
function onEditKey(e: KeyboardEvent) {
  if (e.key === 'Enter') {
    e.preventDefault()
    commitRename()
  } else if (e.key === 'Escape') {
    e.preventDefault()
    editing.value = ''
  }
}

// ── row menu + delete ────────────────────────────────────────
const menuFor = ref('')
const confirmDelete = ref<ChatMeta | null>(null)
const confirmAll = ref(false)
function askDelete(c: ChatMeta) {
  menuFor.value = ''
  confirmDelete.value = c
}
function doDelete() {
  if (confirmDelete.value) deleteChat(confirmDelete.value.id)
  confirmDelete.value = null
}
function doDeleteAll() {
  deleteAllChats()
  confirmAll.value = false
}

// ── keyboard: ↑/↓ move, Enter opens ──────────────────────────
function onListKey(e: KeyboardEvent) {
  if (editing.value || !flat.value.length) return
  const i = flat.value.findIndex((c) => c.id === chatState.activeId)
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault()
    const j = e.key === 'ArrowDown' ? Math.min(i + 1, flat.value.length - 1) : Math.max(i - 1, 0)
    switchChat(flat.value[j].id)
  } else if (e.key === 'Enter') {
    emit('picked')
  }
}

function subtitle(c: ChatMeta): string {
  const model = c.model ? c.model.replace(/^(ollama|hf):/, '').replace(/^.*\//, '') : 'auto'
  return c.count ? `${model} · ${c.count} message${c.count === 1 ? '' : 's'}` : model
}
</script>

<template>
  <aside class="chat-list">
    <div class="chat-list-head">
      <Button label="+ New chat" size="small" severity="secondary" class="chat-list-new" @click="startNew" />
      <input v-model="query" class="chat-list-filter" type="search" placeholder="Filter chats" aria-label="filter chats by title" />
    </div>

    <div class="chat-list-body" role="listbox" tabindex="0" aria-label="chats" @keydown="onListKey">
      <template v-for="g in groups" :key="g.label">
        <div class="chat-list-group mono muted">{{ g.label }}</div>
        <div
          v-for="c in g.chats" :key="c.id"
          class="chat-row" :class="{ active: c.id === chatState.activeId }"
          role="option" :aria-selected="c.id === chatState.activeId"
          @click="editing !== c.id && pick(c.id)" @dblclick="startRename(c)"
        >
          <div class="chat-row-text">
            <input
              v-if="editing === c.id" ref="editInput" v-model="draft" class="chat-row-edit"
              maxlength="120" aria-label="chat title"
              @keydown="onEditKey" @blur="commitRename" @click.stop
            />
            <span v-else class="chat-row-title">{{ c.title }}</span>
            <span class="chat-row-sub mono muted">{{ subtitle(c) }}</span>
          </div>
          <span v-if="chatState.streamingId === c.id" class="chat-row-live" title="answering…" />
          <button
            v-if="c.count > 0" class="chat-row-menu" title="chat actions" aria-label="chat actions"
            @click.stop="menuFor = menuFor === c.id ? '' : c.id"
          >⋯</button>
          <div v-if="menuFor === c.id" class="chat-row-pop" @click.stop>
            <button @click="startRename(c)">Rename</button>
            <button class="danger" @click="askDelete(c)">Delete</button>
          </div>
        </div>
      </template>
      <p v-if="!flat.length" class="muted chat-list-none">No chats match.</p>
    </div>

    <div class="chat-list-foot muted">
      <span v-if="!chatState.storageOK">History is off in this browser (storage blocked).</span>
      <span v-else>Stored in this browser only.</span>
      <span v-if="chatState.notice" class="chat-list-notice">{{ chatState.notice }}</span>
      <button
        v-if="chatState.index.chats.some((c) => c.count > 0)" class="chat-list-clear"
        @click="confirmAll = true"
      >Delete all chats</button>
    </div>

    <Dialog
      :visible="confirmDelete !== null" modal header="Delete chat" :style="{ width: 'min(26rem, 92vw)' }"
      @update:visible="(v: boolean) => { if (!v) confirmDelete = null }"
    >
      <p style="margin: 0">
        Delete “{{ confirmDelete?.title }}”? This removes it from this browser.
      </p>
      <template #footer>
        <Button label="Cancel" severity="secondary" size="small" @click="confirmDelete = null" />
        <Button label="Delete" severity="danger" size="small" @click="doDelete" />
      </template>
    </Dialog>

    <Dialog
      :visible="confirmAll" modal header="Delete all chats" :style="{ width: 'min(26rem, 92vw)' }"
      @update:visible="(v: boolean) => { confirmAll = v }"
    >
      <p style="margin: 0">Delete every chat kept in this browser for this account? This cannot be undone.</p>
      <template #footer>
        <Button label="Cancel" severity="secondary" size="small" @click="confirmAll = false" />
        <Button label="Delete all" severity="danger" size="small" @click="doDeleteAll" />
      </template>
    </Dialog>
  </aside>
</template>
