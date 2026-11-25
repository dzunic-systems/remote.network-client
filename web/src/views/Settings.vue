<script setup lang="ts">
// The Settings tab: every client setting with its effective value and the
// source it resolved from (flag > env > config file > default). A setting
// a flag or an environment variable owns is locked for this run — the
// reason says which. File and default values are editable here (written
// exactly like `config set`); the router URL is never editable from the
// browser, because changing it would send the API key to a new host.
import { onMounted, ref } from 'vue'
import { api, clientState, type ServerView, type SettingView, type ToolView } from '../api'

const settings = ref<SettingView[]>([])
const toolsEnabled = ref(false)
const error = ref('')
const draft = ref<Record<string, string>>({})
const savedKey = ref('')
const restartKeys = ref<string[]>([])

// the tools list (servers and their tools, never their commands)
const toolServers = ref<ServerView[]>([])
const toolDisclosure = ref('')
const toolError = ref('')
const toolsLoading = ref(false)

// replace-key dialog
const keyDialog = ref(false)
const keyDraft = ref('')
const keyError = ref('')
const keyOK = ref(false)

// doctor
const doctorRunning = ref(false)
const doctorReport = ref('')
const doctorOK = ref<boolean | null>(null)

async function refresh() {
  error.value = ''
  try {
    const s = await api.settings()
    settings.value = s.settings
    toolsEnabled.value = s.tools_enabled
    if (s.tools_enabled) void refreshTools()
  } catch (e) {
    error.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  }
}
onMounted(refresh)

// refreshTools lists the configured servers and their tools (this is what
// launches them). Failures show per list; the healthy servers still list.
async function refreshTools() {
  toolsLoading.value = true
  toolError.value = ''
  try {
    const doc = await api.tools()
    toolServers.value = doc.servers
    toolDisclosure.value = doc.disclosure
    if (doc.error) toolError.value = doc.error
  } catch (e) {
    toolError.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  } finally {
    toolsLoading.value = false
  }
}

// setMode flips one tool's allow/ask/deny (the only tool change the page
// may make; servers and their commands are file-only).
async function setMode(t: ToolView, mode: ToolView['mode']) {
  if (t.mode === mode) return
  const before = t.mode
  t.mode = mode
  try {
    await api.setToolMode(t.name, mode)
  } catch (e) {
    t.mode = before
    toolError.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  }
}

function sourceLabel(s: SettingView): string {
  if (s.source === 'flag') return `set by ${s.source_name} (this run)`
  if (s.source === 'env') return `set by ${s.source_name} (this run)`
  if (s.source === 'file') return 'config file'
  if (s.source === 'default') return 'default'
  return 'not set'
}

function display(v: SettingView): string {
  if (v.kind === 'secret') return v.set ? 'set' : 'not set'
  if (v.kind === 'bool') return v.value ? 'on' : 'off'
  if (!v.set) return 'not set'
  return String(v.value ?? '')
}

async function saveString(s: SettingView) {
  error.value = ''
  savedKey.value = ''
  try {
    const r = await api.setSetting(s.key, draft.value[s.key] ?? '')
    await refresh()
    savedKey.value = s.key
    if (r.restart) noteRestart(s.key)
  } catch (e) {
    error.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  }
}

async function saveBool(s: SettingView, on: boolean) {
  error.value = ''
  savedKey.value = ''
  try {
    const r = await api.setSetting(s.key, on)
    await refresh()
    savedKey.value = s.key
    if (r.restart) noteRestart(s.key)
  } catch (e) {
    error.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  }
}

// A few settings only shape the next serve run — say so where the row
// changes one of them.
function noteRestart(key: string) {
  if (!restartKeys.value.includes(key)) restartKeys.value.push(key)
}

function isRestartRow(s: SettingView): boolean {
  return s.key === 'listen' || s.key === 'port'
}

async function replaceKey() {
  keyError.value = ''
  if (!keyDraft.value.trim()) {
    keyError.value = 'paste a key first'
    return
  }
  try {
    await api.replaceKey(keyDraft.value.trim())
    keyDialog.value = false
    keyDraft.value = ''
    keyOK.value = true
    await refresh()
    window.setTimeout(() => { keyOK.value = false }, 4000)
  } catch (e) {
    keyError.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
  }
}

async function runDoctor() {
  doctorRunning.value = true
  doctorReport.value = ''
  doctorOK.value = null
  try {
    const r = await api.doctor()
    doctorReport.value = r.report
    doctorOK.value = r.ok
  } catch (e) {
    doctorReport.value = String((e as Error)?.message ?? e).replace(/^Error: /, '')
    doctorOK.value = false
  } finally {
    doctorRunning.value = false
  }
}

function isAPIKeyRow(s: SettingView): boolean {
  return s.kind === 'secret' && s.key === 'api_key'
}
</script>

<template>
  <div class="settings-page">
    <div v-if="error" class="card gate-card turn-error-card">
      <p class="mono" style="margin: 0; font-size: 13px">{{ error }}</p>
    </div>

    <div class="card">
      <p class="kicker">Settings — flag › environment › config file › default</p>
      <p class="muted" style="font-size: 13px; margin: 0 0 1rem">
        A setting a flag or an environment variable owns is locked while
        serve runs. Everything else is written to the config file, the same
        way <span class="mono">remote-client config set</span> writes it.
      </p>
      <div class="settings-rows">
        <div v-for="s in settings" :key="s.key" class="setting-row">
          <div class="setting-name mono">{{ s.label }}</div>

          <div class="setting-value">
            <!-- Secrets: state only, and the key row gains a replace action. -->
            <template v-if="s.kind === 'secret'">
              <span class="mono" :class="{ muted: !s.set }">{{ display(s) }}</span>
              <span class="src-chip mono" :class="'src-' + s.source">{{ sourceLabel(s) }}</span>
              <template v-if="isAPIKeyRow(s)">
                <button class="setting-action" @click="keyDialog = true">Replace key…</button>
                <span v-if="keyOK" class="mono saved-note">stored (0600)</span>
              </template>
            </template>

            <!-- Bools: a toggle when editable, plain state when locked. -->
            <template v-else-if="s.kind === 'bool'">
              <template v-if="!s.locked">
                <button
                  class="setting-toggle mono" :class="{ on: s.value }"
                  :title="'turns ' + (s.value ? 'off' : 'on')"
                  @click="saveBool(s, !s.value)"
                >{{ display(s) }}</button>
              </template>
              <span v-else class="mono">{{ display(s) }}</span>
              <span class="src-chip mono" :class="'src-' + s.source">{{ sourceLabel(s) }}</span>
            </template>

            <!-- Strings: an inline edit when editable. -->
            <template v-else>
              <template v-if="!s.locked">
                <input
                  v-model="draft[s.key]" class="setting-input mono" :placeholder="display(s)"
                  aria-label="new value" @keydown.enter="saveString(s)"
                />
                <button class="setting-action" @click="saveString(s)">Save</button>
              </template>
              <span v-else class="mono">{{ display(s) }}</span>
              <span class="src-chip mono" :class="'src-' + s.source">{{ sourceLabel(s) }}</span>
            </template>
          </div>

          <div class="setting-note muted">
            <span v-if="s.lock_reason">{{ s.lock_reason }}</span>
            <span v-if="savedKey === s.key" class="saved-note mono">saved</span>
            <span v-if="isRestartRow(s)" class="mono">takes effect on restart — serve does not rebind live</span>
            <span v-if="s.note">{{ s.note }}</span>
          </div>
        </div>
      </div>
    </div>

    <div class="card">
      <p class="kicker">Local tools</p>
      <template v-if="!toolsEnabled">
        <p class="muted" style="font-size: 13px; margin: 0">
          The tool loop is off — turn it on with <span class="mono">tools: on</span> (or
          <span class="mono">config set tools on</span>) and configure servers in the MCP
          config file (its row above). A chat without tools is unaffected.
        </p>
      </template>
      <template v-else>
        <p class="muted" style="font-size: 13px; margin: 0 0 0.75rem">
          Servers come from the MCP config file — the page never adds or repoints one (the
          file names commands this machine runs). What the page may change is a tool's
          permission: <span class="mono">allow</span> runs it without asking,
          <span class="mono">ask</span> prompts every turn (the default),
          <span class="mono">deny</span> refuses it.
        </p>
        <div v-if="toolDisclosure" class="tool-disclosure mono">{{ toolDisclosure }}</div>
        <div v-if="toolError" class="turn-error mono" style="margin: 0.5rem 0">{{ toolError }}</div>
        <p v-if="toolsLoading && !toolServers.length" class="muted mono" style="font-size: 12px">listing tools…</p>
        <div v-for="s in toolServers" :key="s.name" class="server-block">
          <div class="server-name mono">server: {{ s.name }}</div>
          <div v-if="!s.tools.length" class="muted mono" style="font-size: 12px">no tools listed</div>
          <div v-for="t in s.tools" :key="t.name" class="tool-row">
            <div class="tool-row-name mono">{{ t.name }}</div>
            <div class="tool-row-desc muted">{{ t.description }}</div>
            <div class="tool-row-modes">
              <button
                v-for="m in (['allow', 'ask', 'deny'] as const)" :key="m"
                class="mode-pill mono" :class="{ active: t.mode === m, [m]: t.mode === m }"
                @click="setMode(t, m)"
              >{{ m }}</button>
            </div>
          </div>
        </div>
        <button class="setting-action" :disabled="toolsLoading" @click="refreshTools">
          {{ toolsLoading ? 'Listing…' : 'Refresh' }}
        </button>
      </template>
    </div>

    <div class="card">
      <p class="kicker">Check</p>
      <p class="muted" style="font-size: 13px; margin: 0 0 0.75rem">
        The same checks <span class="mono">remote-client doctor</span> runs:
        public address, NAT type, and a QUIC ping of a node that accepts
        direct connections. Sends no inference and costs nothing.
      </p>
      <button class="setting-action" :disabled="doctorRunning" @click="runDoctor">
        {{ doctorRunning ? 'Checking…' : 'Run check' }}
      </button>
      <pre v-if="doctorReport" class="doctor-report mono" :class="{ 'doctor-bad': doctorOK === false }">{{ doctorReport }}</pre>
    </div>

    <Dialog
      v-model:visible="keyDialog" modal header="Replace the API key" :style="{ width: 'min(26rem, 92vw)' }"
    >
      <p class="muted" style="margin: 0 0 .6rem; font-size: 13px">
        The new key is stored in the key file with owner-only permissions.
        It is never shown back. Until serve restarts, requests keep using
        the key this run started with.
      </p>
      <input
        v-model="keyDraft" class="setting-input mono" style="width: 100%"
        type="password" placeholder="paste the new key" aria-label="new API key"
        @keydown.enter="replaceKey"
      />
      <p v-if="keyError" class="turn-error mono">{{ keyError }}</p>
      <template #footer>
        <Button label="Cancel" severity="secondary" size="small" @click="keyDialog = false" />
        <Button label="Store key" size="small" @click="replaceKey" />
      </template>
    </Dialog>
  </div>
</template>
