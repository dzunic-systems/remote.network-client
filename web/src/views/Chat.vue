<script setup lang="ts">
// The Chat tab: streaming chat against any live model on the network,
// over the local endpoint (direct to a node when a direct path exists,
// through the router otherwise). Ported from the router dashboard's
// Chat.vue: the accounts, the node pinning, the document uploads and the
// router curl sample are gone — what stays is the model picker, the
// thinking-effort picker, the web-search toggle, image attachments and
// the multi-chat list. The stream state lives in chat.ts (module scope) —
// switching tabs mid-answer does not kill the turn; coming back
// re-attaches.
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, type ModelCard } from '../api'
import {
  restoreChats, sendChat, stopStreaming, newChat, useChatState, openInChat,
  activeMeta, setWebSearch,
} from '../chat'
import ChatList from '../components/ChatList.vue'
import { renderMarkdown } from '../lib/md.js'

const chatState = useChatState()

const models = ref<ModelCard[]>([])
const modelsErr = ref('')
const input = ref('')
const logEl = ref<HTMLElement | null>(null)
let timer: number | undefined

// streaming: a turn runs somewhere (one at a time — Send waits);
// streamingHere: it runs in the chat on screen (cursor, Stop, locked menus).
const streaming = computed(() => chatState.streaming)
const streamingHere = computed(() => chatState.streaming && chatState.streamingId === chatState.activeId)
const listOpen = ref(false) // the chat list drawer on narrow screens
const currentTitle = computed(() =>
  chatState.index.chats.find((c) => c.id === chatState.activeId)?.title ?? 'New chat')

const chatModels = computed(() => {
  const chat = models.value.filter((m) => m.modality !== 'embedding' && m.specialty !== 'ocr')
  return chat.sort((a, b) =>
    (b.live_nodes - a.live_nodes) || (b.featured ? 1 : 0) - (a.featured ? 1 : 0) ||
    a.display_name.localeCompare(b.display_name))
})

const modelOptions = computed(() => {
  const opts = chatModels.value.map((m) => ({
    value: m.id,
    label: `${m.display_name}${m.vision ? ' · vision' : ''}${m.tools ? ' · tools' : ''}${m.live_nodes > 0 ? '' : ' (offline)'}`,
  }))
  if (chatState.model && !opts.some((o) => o.value === chatState.model)) {
    const card = models.value.find((m) => m.id === chatState.model)
    const label = card?.modality === 'embedding'
      ? `${card.display_name} (embedding model — not for chat)`
      : card?.specialty === 'ocr' ? `${card.display_name} (OCR — attach an image)` : chatState.model
    opts.unshift({ value: chatState.model, label })
  }
  return opts
})

async function refreshModels() {
  try {
    const m = await api.models()
    models.value = m.models
    modelsErr.value = ''
  } catch (e) {
    modelsErr.value = String(e)
  }
}

onMounted(async () => {
  restoreChats()
  // A ?model= link opens that model (in a fresh chat when this one has
  // history), once: the key is consumed so coming back to the tab later
  // does not open it again.
  const u = new URL(window.location.href)
  const linked = u.searchParams.get('model')
  if (linked) {
    if (linked !== chatState.model) openInChat(linked)
    u.searchParams.delete('model')
    window.history.replaceState(null, '', u)
  }
  window.addEventListener('keydown', onGlobalKey)
  await refreshModels()
  // Auto-pick: deep link first, then the best live model.
  if (!chatState.model && chatModels.value.length) {
    chatState.model = (chatModels.value.find((m) => m.live_nodes > 0 && m.featured)
      ?? chatModels.value.find((m) => m.live_nodes > 0)
      ?? chatModels.value[0]).id
  }
  timer = window.setInterval(refreshModels, 5000)
  scrollToBottom()
})
onUnmounted(() => {
  clearInterval(timer)
  window.removeEventListener('keydown', onGlobalKey)
})

// Ctrl/Cmd+Shift+O: a new chat (only while the Chat tab is open).
function onGlobalKey(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.shiftKey && e.key.toLowerCase() === 'o') {
    e.preventDefault()
    newChat()
  }
}

function scrollToBottom() {
  nextTick(() => {
    if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
  })
}
watch(() => chatState.messages.map((m) => m.text.length).join(), scrollToBottom)

// The 🌐 toggle defaults to what the selected model can do: a tool-capable
// card starts with the agentic search loop on; a non-tool card starts off
// (flipping it on still works — pre-generation search serves cards without
// function calling). Re-fires when the directory refresh flips a card's
// tools flag; an id the directory doesn't list leaves the toggle alone.
watch(
  () => {
    const m = models.value.find((x) => x.id === chatState.model)
    return m ? `${m.id}|${m.tools}` : ''
  },
  (key) => {
    // A chat whose user set the toggle keeps it; others follow the model.
    if (key && activeMeta()?.webSearch == null) chatState.webSearch = key.endsWith('|true')
  },
)
// Switching to a chat that follows the default re-applies it (the model
// may be the same, so the watcher above would not fire).
watch(() => chatState.activeId, () => {
  const m = models.value.find((x) => x.id === chatState.model)
  if (m && activeMeta()?.webSearch == null) chatState.webSearch = !!m.tools
})

// ── attachments: images pasted/dropped/picked ───────────────────────
const pendingImages = ref<string[]>([])
const dropping = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

const selectedModel = computed(() => models.value.find((m) => m.id === chatState.model))

// The thinking-effort choices. When the selected card advertises its own
// vocabulary, offer exactly that; otherwise the common set. '' sends
// nothing and leaves the node's default.
const FALLBACK_EFFORTS = ['off', 'low', 'medium', 'high', 'xhigh']
const effortOptions = computed(() => {
  const efforts = selectedModel.value?.reasoning_efforts?.length
    ? selectedModel.value.reasoning_efforts
    : FALLBACK_EFFORTS
  return [{ value: '', label: 'thinking: default' },
    ...efforts.map((e) => ({ value: e, label: 'thinking: ' + e }))]
})
// A model switch that drops the current selection resets it — sending a
// rejected effort would 400 every round.
watch(effortOptions, (opts) => {
  if (!opts.some((o) => o.value === chatState.effort)) chatState.effort = ''
})
// An embedding model cannot chat: the proxy answers 400 model_not_chat.
// Reached by "Try" on a card or a ?model= link (the picker itself lists
// chat models only) — say so before the turn.
const notChat = computed(() => selectedModel.value?.modality === 'embedding')
// An OCR model reads the text in images; given text alone it loops on its
// own prompt markers (the proxy answers 400 model_needs_image). It chats
// once an image is attached here or earlier in the conversation.
const ocrNeedsImage = computed(() =>
  selectedModel.value?.specialty === 'ocr' && pendingImages.value.length === 0 &&
  !chatState.messages.some((m) => m.images?.length))

const visionMismatch = computed(() =>
  pendingImages.value.length > 0 && selectedModel.value != null && !selectedModel.value.vision)

const isImage = (f: File) => f.type.startsWith('image/')

async function addFiles(files: Iterable<File>) {
  for (const f of files) {
    if (isImage(f)) {
      if (f.size > 8 << 20) {
        chatState.error = `${f.name} is over 8 MB — too large to attach`
        continue
      }
      const dataURL = await new Promise<string>((resolve, reject) => {
        const r = new FileReader()
        r.onload = () => resolve(String(r.result))
        r.onerror = () => reject(r.error)
        r.readAsDataURL(f)
      })
      pendingImages.value.push(dataURL)
    } else {
      chatState.error = `${f.name}: this chat takes images (documents belong in the prompt text)`
    }
  }
}

function onPaste(e: ClipboardEvent) {
  const files = Array.from(e.clipboardData?.files ?? [])
  if (files.length) {
    e.preventDefault()
    addFiles(files)
  }
}
function onDrop(e: DragEvent) {
  dropping.value = false
  const files = Array.from(e.dataTransfer?.files ?? [])
  if (files.length) {
    e.preventDefault()
    addFiles(files)
  }
}
function onDragOver(e: DragEvent) {
  if (Array.from(e.dataTransfer?.types ?? []).includes('Files')) {
    e.preventDefault() // required or the drop event never fires
    dropping.value = true
  }
}
function pickFiles() { fileInput.value?.click() }
function onFilesPicked(e: Event) {
  const input = e.target as HTMLInputElement
  addFiles(Array.from(input.files ?? []))
  input.value = ''
}

async function send() {
  const text = input.value
  const images = pendingImages.value
  input.value = ''
  pendingImages.value = []
  scrollToBottom()
  await sendChat(text, images)
  scrollToBottom()
}

function onKeydown(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
    e.preventDefault()
    if (canSend.value) send()
  }
}

const canSend = computed(() =>
  !streaming.value &&
  (input.value.trim() !== '' || pendingImages.value.length > 0) &&
  !notChat.value && !ocrNeedsImage.value)
</script>

<template>
  <div class="chat-layout">
    <ChatList class="chat-side" :class="{ open: listOpen }" @picked="listOpen = false" />
    <div v-if="listOpen" class="chat-scrim" @click="listOpen = false" />
    <div class="chat-main">
      <!-- Narrow screens: the chat list lives in a drawer; this bar opens it
           and names the chat on screen. -->
      <div class="chat-mobile-bar">
        <button
          class="chat-list-toggle" title="your chats" @click="listOpen = true"
        >☰ Chats</button>
        <span class="chat-current-title" :title="currentTitle">{{ currentTitle }}</span>
      </div>
      <div class="chat-head">
        <Select
          v-model="chatState.model" :options="modelOptions" option-label="label" option-value="value"
          placeholder="pick a model" style="min-width: 16rem" :disabled="streamingHere"
        />
        <Select
          v-model="chatState.effort" :options="effortOptions" option-label="label" option-value="value"
          placeholder="thinking: default"
          style="min-width: 10rem" :disabled="streamingHere"
          title="thinking effort — how much the model may reason before answering; a thinking card's default can be slow"
        />
        <Button
          :label="chatState.webSearch ? '🌐 search on' : '🌐 search off'"
          :severity="chatState.webSearch ? 'success' : 'secondary'"
          size="small" :disabled="streamingHere" title="search the web for each turn and ground the answer in the sources (the search runs on the router)"
          @click="setWebSearch(!chatState.webSearch)"
        />
        <Button label="New chat" severity="secondary" size="small" class="chat-new-narrow" @click="newChat()" />
      </div>

      <div
        ref="logEl" class="chat-log" :class="{ dropping }"
        @dragover="onDragOver" @dragleave="dropping = false" @drop="onDrop"
      >
        <p v-if="!chatState.messages.length" class="muted chat-empty">
          Ask anything. Replies stream over the local endpoint — straight to
          a serving node when a direct path exists, through a router
          otherwise. Paste or drop images for vision models.
          <span v-if="modelsErr"><br />could not list models — {{ modelsErr }}</span>
        </p>
        <div v-for="(m, i) in chatState.messages" :key="i" class="msg" :class="'msg-' + m.role">
          <div v-if="m.role === 'assistant' && (m.searchQueries?.length || m.searchQuery || m.searchNote)" class="msg-search mono">
            <span v-if="m.searchQueries?.length || m.searchQuery">🔎 {{ (m.searchQueries ?? (m.searchQuery ? [m.searchQuery] : [])).join(' · ') }}</span>
            <span v-if="m.searchNote" :class="{ muted: m.searchQueries?.length || m.searchQuery }">— {{ m.searchNote }}</span>
          </div>
          <div v-if="m.role === 'assistant'" class="msg-meta mono muted">
            {{ m.model }}
            <details v-if="m.reasoning" class="reasoning">
              <summary class="mono muted">thinking…</summary>
              <div class="reasoning-body mono">{{ m.reasoning }}</div>
            </details>
          </div>
          <div
            v-if="m.role === 'assistant'" class="msg-body md"
            v-html="renderMarkdown(m.text) || (streamingHere && i === chatState.messages.length - 1 ? '<span class=&quot;cursor&quot;>▌</span>' : '')"
          />
          <div v-else class="msg-body">
            <div v-if="m.images?.length" class="msg-images">
              <img v-for="(src, j) in m.images" :key="j" :src="src" alt="attached" class="msg-image" />
            </div>
            <div v-else-if="m.imageCount" class="msg-docs mono">
              <span class="msg-doc muted" title="images are not kept in browser storage">🖼 {{ m.imageCount }} image{{ m.imageCount > 1 ? 's' : '' }} not kept</span>
            </div>
            {{ m.text }}
          </div>
          <details v-if="m.role === 'assistant' && m.search?.length" class="msg-sources">
            <!-- Collapsed like the thinking block: 30+ links while the answer
                 streams pushed the text below the fold. -->
            <summary class="mono muted">sources ({{ m.search.length }})</summary>
            <a v-for="(s, j) in m.search" :key="j" :href="s.url" target="_blank" rel="noopener" class="source-link mono">
              [{{ j + 1 }}] {{ s.title || s.url }}
            </a>
          </details>
          <div v-if="m.error" class="turn-error mono">{{ m.error }}</div>
        </div>

        <div v-if="chatState.error" class="card gate-card turn-error-card">
          <p class="kicker">That turn failed</p>
          <p class="mono" style="margin: 0; font-size: 13px">{{ chatState.error }}</p>
        </div>
      </div>

      <div v-if="dropping" class="drop-hint mono">drop images to attach</div>
      <div v-if="pendingImages.length" class="pending-images">
        <span v-for="(src, j) in pendingImages" :key="'i' + j" class="pending-image">
          <img :src="src" alt="pending" />
          <button class="pending-remove" title="remove" @click="pendingImages.splice(j, 1)">×</button>
        </span>
        <span v-if="visionMismatch" class="muted vision-warn">
          {{ selectedModel?.display_name }} may not see images — pick a model tagged vision
        </span>
      </div>

      <div v-if="notChat" class="not-chat">
        <strong>{{ selectedModel?.display_name }} is an embedding model — it does not chat.</strong>
        It turns text into vectors (search, similarity, clustering). Pick a chat model above, or call
        the local embeddings endpoint: <span class="mono">POST /v1/embeddings</span>.
      </div>

      <div v-if="ocrNeedsImage" class="not-chat">
        <strong>{{ selectedModel?.display_name }} is an OCR model — it reads the text in images.</strong>
        Attach an image (a page, a scan, a photo) with the paperclip, or paste or drop it here, to
        transcribe it; for questions and conversation, pick a chat model above.
      </div>

      <div class="composer">
        <input ref="fileInput" type="file" accept="image/*" multiple style="display: none" @change="onFilesPicked" />
        <Button
          label="📎" severity="secondary" class="composer-attach" :disabled="streamingHere"
          title="attach images" aria-label="attach images" @click="pickFiles"
        />
        <textarea
          v-model="input" class="composer-input" rows="2"
          placeholder="Message the model… (Cmd/Ctrl+Enter to send; paste or drop images)"
          :disabled="streamingHere" @keydown="onKeydown" @paste="onPaste"
        />
        <Button
          v-if="!streamingHere" label="Send" :disabled="!canSend" @click="send"
          :title="streaming ? 'another chat is still answering' : undefined"
        />
        <Button v-else label="Stop" severity="danger" @click="stopStreaming()" />
      </div>
    </div>
  </div>
</template>
