// Types for chatStorage.js.
import type { ChatIndex, ChatMessage } from './chats.js'

export interface StorageLike {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface ChatStorage {
  ok: boolean
  loadIndex(): ChatIndex | null
  saveIndex(index: ChatIndex, evict?: () => boolean): boolean
  loadMessages(id: string): ChatMessage[]
  saveMessages(id: string, messages: ChatMessage[], evict?: () => boolean): boolean
  deleteMessages(id: string): void
}

export function makeStorage(ls?: StorageLike | null): ChatStorage
