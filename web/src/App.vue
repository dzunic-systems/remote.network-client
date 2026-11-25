<script setup lang="ts">
import { onMounted, ref } from 'vue'
import Chat from './views/Chat.vue'
import Settings from './views/Settings.vue'
import { restoreChats } from './chat'
import { needsToken, storedToken, saveToken } from './api'

// Two tabs; ?tab= deep-links (the chat keeps its ?chat= while open).
type TabID = 'chat' | 'settings'
const tabs: { id: TabID; label: string }[] = [
  { id: 'chat', label: 'Chat' },
  { id: 'settings', label: 'Settings' },
]

function stateFromURL(): TabID {
  try {
    return new URLSearchParams(window.location.search).get('tab') === 'settings' ? 'settings' : 'chat'
  } catch {
    return 'chat'
  }
}

const tab = ref<TabID>(stateFromURL())

function syncURL() {
  try {
    const u = new URL(window.location.href)
    if (tab.value === 'chat') u.searchParams.delete('tab')
    else u.searchParams.set('tab', tab.value)
    if (tab.value !== 'chat') u.searchParams.delete('chat') // chat.ts owns it while Chat is open
    window.history.replaceState(null, '', u)
  } catch { /* no window (tests) */ }
}

function setTab(t: TabID) {
  tab.value = t
  syncURL()
}

onMounted(() => {
  syncURL() // canonical form on boot
  restoreChats()
})

// A non-loopback listener requires the local token: the page asks once
// and keeps it in sessionStorage (a new tab starts empty by design).
const tokenDraft = ref('')
function submitToken() {
  if (!tokenDraft.value.trim()) return
  saveToken(tokenDraft.value.trim())
  tokenDraft.value = ''
  needsToken.value = false
  window.location.reload() // every surface re-runs with the token
}
function tokenKnown(): boolean {
  return storedToken() !== ''
}
</script>

<template>
  <div class="page">
    <header class="masthead">
      <a class="brand" href="https://remote.network" aria-label="remote.network home">
        <svg class="mark" viewBox="0 0 32 32" aria-hidden="true">
          <path d="M14.1 10.9 9.4 20.1M17.9 10.9l4.7 9.2M10.4 23.4h11.2" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>
          <circle cx="16" cy="8" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6"/>
          <circle cx="7" cy="23.5" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6"/>
          <circle cx="25" cy="23.5" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6"/>
        </svg>
        <span class="brand-name">remote</span><span class="brand-tld">.network</span>
      </a>
      <span class="mast-kicker"><span class="pulse"></span>local endpoint — beta</span>
      <a class="site-link" href="https://remote.network">site ↗</a>
    </header>
    <nav class="tabs" aria-label="Client sections">
      <button
        v-for="t in tabs" :key="t.id"
        class="tab" :class="{ active: tab === t.id }"
        @click="setTab(t.id)"
      >{{ t.label }}</button>
    </nav>
    <Chat v-if="tab === 'chat'" />
    <Settings v-else />

    <Dialog
      :visible="needsToken" modal header="This endpoint needs its token" :style="{ width: 'min(26rem, 92vw)'}"
      :closable="false"
    >
      <p style="margin: 0 0 .6rem">
        serve is listening on a non-loopback address and every caller must
        present the local token<span v-if="tokenKnown()"> — the one stored in this
        tab was refused</span>. Paste it once; it stays in this tab only.
      </p>
      <input
        v-model="tokenDraft" type="password" class="setting-input mono" style="width: 100%"
        placeholder="the --local-token value" aria-label="local token" @keydown.enter="submitToken"
      />
      <template #footer>
        <Button label="Use this token" size="small" :disabled="!tokenDraft.trim()" @click="submitToken" />
      </template>
    </Dialog>
  </div>
</template>
