// The online way of a title page's «Смотреть ▾» and what the page and the
// episode guide use around it: whether the way is offered (`useAvailable`,
// answered from x/available, asked when the bundle loads and kept for a
// minute), the picker as the way's sheet, the guide's picker on an episode,
// and the page panel that warms a title's sources and sends kept positions.
import { useUser, type ItemDetail } from '@antresol/addon-sdk'
import { useCallback, useEffect, useState, useSyncExternalStore, type ReactElement, type ReactNode } from 'react'
import { ID, onlineAvailable, onlineSources } from './api'
import { OnlineSheet } from './OnlineSheet'
import { usePendingRetry } from './pendingProgress'

/** The way's id in «Смотреть ▾» (`<addonId>:<label>`, the label is the strings key `way`). */
export const WATCH_WAY = `${ID}:way`

/** A title page waits this long before warming its online sources (a quick pass-through does not ask). */
const WARM_DELAY_MS = 800

// ---------- whether the way is offered ----------

/** How long an answer is kept, and how soon a failed ask is made again. */
export const AVAILABLE_TTL_MS = 60_000
export const AVAILABLE_RETRY_MS = 10_000

// the one answer every title shares: «Смотреть онлайн» is on and the sources
// are configured (the rating is per title, checked when the picker opens);
// null until the first answer
let known: boolean | null = null
let checkedAt = 0
let lastAskFailed = false
let asking: Promise<void> | null = null
const listeners = new Set<() => void>()
const subscribe = (l: () => void) => {
  listeners.add(l)
  return () => void listeners.delete(l)
}
const snapshot = () => known

function remember(v: boolean | null) {
  if (v === known) return
  known = v
  for (const l of listeners) l()
}

/**
 * Asks x/available unless an answer is fresh (a minute; ten seconds after a
 * failed ask) or one is on its way. Called when the bundle loads, so the
 * answer is there before a title page needs it; the answer a failed ask
 * leaves is the last one known (none yet: not offered).
 */
export function prefetchAvailable(): Promise<void> {
  if (asking) return asking
  if (known !== null && Date.now() - checkedAt < (lastAskFailed ? AVAILABLE_RETRY_MS : AVAILABLE_TTL_MS)) return Promise.resolve()
  asking = onlineAvailable().then(
    (v) => settle(v, false),
    () => settle(known ?? false, true),
  )
  return asking
}

function settle(answer: boolean, failed: boolean) {
  asking = null
  checkedAt = Date.now()
  lastAskFailed = failed
  remember(answer)
}

/**
 * Asks again now (the admin changed the setting): the answer in use stays
 * until the new one arrives. An ask already on its way may have been sent
 * before the change, so its answer is not taken for the new one: the next
 * ask follows it.
 */
export function refreshAvailable(): Promise<void> {
  const again = () => {
    checkedAt = 0
    return prefetchAvailable()
  }
  return asking ? asking.then(again) : again()
}

/** Forgets the answer (tests). */
export function resetAvailableForTests() {
  known = null
  checkedAt = 0
  lastAskFailed = false
  asking = null
  for (const l of listeners) l()
}

/**
 * The way's `useAvailable`: `null` while the first answer is on its way (the
 * way is not offered yet), then whether «Смотреть онлайн» is on and
 * configured, for a movie or a series with a TMDB id. It runs in a
 * component of the app's own (no context of this addon): it reads the
 * module's answer only.
 */
export function useAvailable(t: { kind: string; tmdbId: number }): boolean | null {
  const answer = useSyncExternalStore(subscribe, snapshot, snapshot)
  // refreshes a stale answer; the old one stays meanwhile, nothing moves
  useEffect(() => void prefetchAvailable(), [])
  if (t.tmdbId <= 0 || (t.kind !== 'movie' && t.kind !== 'series')) return false
  return answer
}

// ---------- the way, the picker on an episode, the warm-up ----------

/** The kind of a title the picker takes (null: not a movie or a series). */
const kindOf = (kind: string): 'movie' | 'series' | null => (kind === 'movie' || kind === 'series' ? kind : null)

/**
 * The way's `run`: the picker as the sheet the app draws until it closes
 * (nothing for a title without a TMDB id).
 */
export function runWay(item: ItemDetail, close: () => void): ReactElement | undefined {
  const kind = kindOf(item.kind)
  const tmdbId = item.tmdbId ?? 0
  if (!kind || tmdbId <= 0) return undefined
  return <OnlineSheet kind={kind} tmdbId={tmdbId} title={item.title} poster={item.poster || undefined} onClose={close} />
}

export interface OnlineSheetTarget {
  kind: 'movie' | 'series'
  tmdbId: number
  title: string
  poster?: string
}

/**
 * The picker for the episode guide's «Другие источники…»: `available` when
 * the way is offered for this title, `openAt` opens the picker on that
 * episode (over the remembered one), and the caller renders `sheet`.
 */
export function useOnlineSheet({ kind, tmdbId, title, poster }: OnlineSheetTarget): {
  available: boolean
  openAt: (at: { season: number; episode: number }) => void
  sheet: ReactNode
} {
  const available = useAvailable({ kind, tmdbId }) === true
  const [at, setAt] = useState<{ season: number; episode: number } | null>(null)
  const openAt = useCallback((a: { season: number; episode: number }) => setAt(a), [])
  const sheet = available && at ? <OnlineSheet kind={kind} tmdbId={tmdbId} title={title} poster={poster} at={at} onClose={() => setAt(null)} /> : null
  return { available, openAt, sheet }
}

/**
 * A panel of an item's and a title's page that draws nothing. It asks the
 * server for a title's sources in the background while the page is read (a
 * title off the library, not every search card), so the picker opens on a
 * cached list instead of a 3–10 s search; and, with the profile it knows,
 * sends the online positions the server could not take earlier.
 */
export function WarmSources({ item }: { item: ItemDetail }): null {
  const kind = kindOf(item.kind)
  const tmdbId = item.tmdbId ?? 0
  usePendingRetry(useUser()?.id ?? 0)
  const offered = useAvailable({ kind: item.kind, tmdbId }) === true
  const warm = item.id === 0 && offered
  useEffect(() => {
    if (!warm || !kind) return
    // not aborted on leaving: the server keeps what it found (6 h) for the next visit
    const timer = window.setTimeout(() => void onlineSources(kind, tmdbId).catch(() => {}), WARM_DELAY_MS)
    return () => window.clearTimeout(timer)
  }, [warm, kind, tmdbId])
  return null
}
