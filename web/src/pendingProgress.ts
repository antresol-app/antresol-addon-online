// Online positions the server could not take yet (503, a restart, offline):
// kept on this device per profile and sent again later. Each one carries
// the profile it belongs to and when it was reached (`updatedAt`), so it is
// only ever sent under that profile, and the server ignores it when it
// already has a newer position (another device).
//
// A send goes out under the session cookie of whoever is signed in, and the
// app tells the addon nothing outside React about who that is. So nothing is
// sent from a timer of its own: a position is sent again only by a component
// that knows the profile at that moment (`usePendingRetry`, the player's
// mount), for that profile's entries alone. A position of another profile is
// left alone until its profile is there again, and one older than a day is
// dropped whoever's it is.
import { ApiError } from '@antresol/addon-sdk'
import { useEffect } from 'react'
import { saveOnlineProgress } from './api'
import type { OnlineProgressSave } from './types'

const KEY = 'vault.online.pending'
export const RETRY_MS = 15_000
/** A position older than this is no longer worth sending. */
export const MAX_AGE_MS = 24 * 3600_000

interface Pending {
  userId: number
  /** RFC 3339: when the position was reached. */
  savedAt: string
  save: OnlineProgressSave
}

const keyOf = (userId: number, p: OnlineProgressSave) => `${userId}:${p.kind}:${p.tmdbId}:${p.season}:${p.episode}`

function read(): Record<string, Pending> {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? '{}') as unknown
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, Pending>) : {}
  } catch {
    return {}
  }
}
function write(all: Record<string, Pending>) {
  try {
    if (Object.keys(all).length === 0) localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, JSON.stringify(all))
  } catch {
    /* private mode: nothing kept */
  }
}

const fresh = (x: Pending) => Date.now() - Date.parse(x.savedAt) < MAX_AGE_MS

/** Positions of profile `userId` kept on this device that the server has not got yet (none older than a day). */
export const hasPending = (userId: number) => userId > 0 && Object.values(read()).some((x) => x.userId === userId && fresh(x))

/** The server or the network is down for now: worth sending again. */
const transient = (e: unknown) => e instanceof ApiError && (e.status === 0 || e.status >= 500)

/** Saves an online position of `userId`; a transient failure keeps it for that profile, to be sent again by usePendingRetry or the next play. */
export async function saveOnline(userId: number, p: OnlineProgressSave, keepalive = false): Promise<void> {
  const savedAt = new Date().toISOString()
  try {
    await saveOnlineProgress(p, keepalive)
    const all = read()
    if (all[keyOf(userId, p)]) {
      delete all[keyOf(userId, p)]
      write(all)
    }
  } catch (e) {
    if (!transient(e) || userId <= 0) return
    write({ ...read(), [keyOf(userId, p)]: { userId, savedAt, save: p } })
  }
}

// one flush at a time for a profile, with every caller that asked for it
const flights = new Map<number, { done: Promise<void>; callers: Set<() => boolean> }>()

/**
 * Sends `userId`'s kept positions (with when they were reached); others'
 * stay untouched. One flush at a time per profile. `current` says whether
 * the caller still knows that profile (its component is mounted, its
 * effect not cleaned up): it is asked before every send, and the flush
 * stops, leaving what is not sent kept, once no caller that asked for it
 * is current. A sign-out or a profile switch during a slow send thus
 * never lets the next position go out under the new session.
 */
export function flushPending(userId: number, current: () => boolean): Promise<void> {
  const flight = flights.get(userId)
  if (flight) {
    flight.callers.add(current)
    return flight.done
  }
  const callers = new Set([current])
  const done = flush(userId, () => [...callers].some((c) => c())).finally(() => flights.delete(userId))
  flights.set(userId, { done, callers })
  return done
}

async function flush(userId: number, current: () => boolean): Promise<void> {
  for (const [k, x] of Object.entries(read())) {
    const drop = () => {
      const all = read()
      if (all[k]?.savedAt === x.savedAt) {
        delete all[k]
        write(all)
      }
    }
    // too old to be worth sending: gone, whoever's it is
    if (!fresh(x)) {
      drop()
      continue
    }
    if (userId <= 0 || x.userId !== userId) continue
    // the caller may have gone (signed out, another profile) while the last one was on its way
    if (!current()) return
    try {
      await saveOnlineProgress({ ...x.save, updatedAt: x.savedAt })
      drop()
    } catch (e) {
      // the server or the network is still down: it stays for the next try
      if (transient(e)) return
      drop()
    }
  }
}

/**
 * While the calling component is mounted: sends profile `userId`'s kept
 * positions now and every 15 s (nothing for profile 0: nobody is known). The
 * profile is the one this component's render knows, so a profile that
 * changed since a position was kept is never sent under the new one; and a
 * send that is under way when the component goes sends no further position.
 */
export function usePendingRetry(userId: number): void {
  useEffect(() => {
    if (userId <= 0) return
    let live = true
    const send = () => {
      if (hasPending(userId)) void flushPending(userId, () => live)
    }
    send()
    const id = window.setInterval(send, RETRY_MS)
    return () => {
      live = false
      window.clearInterval(id)
    }
  }, [userId])
}
