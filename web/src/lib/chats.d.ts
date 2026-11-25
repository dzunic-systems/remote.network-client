// Types for chats.js (the md.d.ts pattern: plain JS so node:test imports it).

export interface SearchSource {
  title: string
  url: string
}

export interface Attachment {
  id: string
  name: string
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  text: string
  reasoning?: string // thinking models (delta.reasoning_content)
  model?: string
  error?: string // this turn failed mid-way; text may be partial
  images?: string[] // data URLs — this turn carried images (never stored)
  imageCount?: number // images the turn carried, once stripped for storage
  docs?: Attachment[] // attached documents — the model reads them itself
  search?: SearchSource[] // web search sources (assistant turns)
  searchQuery?: string
  searchQueries?: string[]
  searchNote?: string
}

export interface ChatMeta {
  id: string
  title: string
  titleSet: boolean // renamed by the user: never auto-retitled
  created: number
  updated: number
  model: string // '' = auto-pick on first send
  effort: string
  webSearch: boolean | null // null = follow the model's default
  count: number
}

export interface ChatIndex {
  v: 1
  active: string
  chats: ChatMeta[] // newest-updated first
}

export const SCHEMA: 1
export const MAX_CHATS: number
export const MAX_STORED: number
export const NEW_TITLE: string
export function emptyIndex(): ChatIndex
export function newID(): string
export function newMeta(now: number, defaults?: { model?: string; effort?: string }): ChatMeta
export function deriveTitle(text: string | undefined, hasImages: boolean, docNames?: string[]): string
export function touch(index: ChatIndex, id: string, now: number): void
export function removeChat(index: ChatIndex, id: string): string
export function evict(index: ChatIndex, keep?: string[], max?: number): string[]
export function oldestFirst(index: ChatIndex, keep?: string[]): string[]
export function slimForStorage(messages: ChatMessage[], max?: number): ChatMessage[]
export function groupByAge(chats: ChatMeta[], now: number): { label: string; chats: ChatMeta[] }[]
export function migrateLegacy(
  raw: unknown, now: number, defaults?: { model?: string; effort?: string },
): { meta: ChatMeta; messages: ChatMessage[] } | null
export function filterChats(chats: ChatMeta[], q: string): ChatMeta[]
