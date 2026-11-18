import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook } from '@testing-library/react'
import { flushPending, hasPending, saveOnline, usePendingRetry } from '../pendingProgress'
import type { OnlineProgressSave } from '../types'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const save: OnlineProgressSave = { kind: 'movie', tmdbId: 77, season: 0, episode: 0, positionMs: 60_000, durationMs: 600_000, source: 'alpha', voice: 'Дубляж', quality: '720p' }

/** PUT x/progress answers from `answers` in turn (then 204). */
function stub(answers: (number | 'lost')[]) {
  const puts: OnlineProgressSave[] = []
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const u = String(input)
    if (u.endsWith('/api/addons/online/x/progress') && init?.method === 'PUT') {
      puts.push(JSON.parse(String(init.body)) as OnlineProgressSave)
      const code = answers.shift() ?? 204
      if (code === 'lost') throw new TypeError('network')
      return code === 204 ? new Response(null, { status: 204 }) : json({ error: 'x' }, code)
    }
    return json({})
  })
  vi.stubGlobal('fetch', f)
  return puts
}
const kept = () => JSON.parse(localStorage.getItem('vault.online.pending') ?? '{}') as Record<string, { userId: number; savedAt: string }>
const entry = (userId: number, savedAt: string) => ({ [`${userId}`]: { userId, savedAt, save } })

describe('online progress kept on this device', () => {
  // fake timers for every case: a timer the module might schedule is the test's to see
  beforeEach(() => {
    localStorage.clear()
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  it('a 503 or 500 keeps the position for its profile and sends it later with when it was reached', async () => {
    const puts = stub([503, 500])
    await saveOnline(2, save)
    expect(Object.values(kept())).toEqual([expect.objectContaining({ userId: 2 })])
    const at = Object.values(kept())[0]!.savedAt
    await flushPending(2, () => true) // a 500: still kept
    expect(Object.keys(kept())).toHaveLength(1)
    await flushPending(2, () => true)
    expect(puts.at(-1)).toEqual({ ...save, updatedAt: at })
    expect(kept()).toEqual({})
  })

  it('a position lost on the way (offline, a save as the page leaves) is kept too', async () => {
    const puts = stub(['lost'])
    await saveOnline(2, save, true)
    expect(hasPending(2)).toBe(true)
    await flushPending(2, () => true)
    expect(puts).toHaveLength(2)
    expect(hasPending(2)).toBe(false)
  })

  it('a refusal that will not change (a 4xx) is not kept', async () => {
    stub([404])
    await saveOnline(2, save)
    expect(hasPending(2)).toBe(false)
  })

  it('nobody signed in (profile 0): nothing is kept', async () => {
    stub([503])
    await saveOnline(0, save)
    expect(hasPending(0)).toBe(false)
    expect(localStorage.getItem('vault.online.pending')).toBeNull()
  })

  it('nothing is sent from a timer of its own: only a component that knows the profile sends again', async () => {
    // a fresh copy of the module, so no timer or flight an earlier case left in it can hide one
    vi.resetModules()
    const fresh = await import('../pendingProgress')
    const puts = stub([503])
    await fresh.saveOnline(2, save)
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    expect(puts).toHaveLength(1) // the failed save itself
    expect(fresh.hasPending(2)).toBe(true)
  })

  it('usePendingRetry sends the profile’s kept positions now and every 15 s until they are out, and stops when it unmounts', async () => {
    const puts = stub([503, 500, 204])
    await saveOnline(2, save)
    const view = renderHook(({ id }) => usePendingRetry(id), { initialProps: { id: 2 } })
    await vi.advanceTimersByTimeAsync(0)
    expect(puts).toHaveLength(2) // on mount: a 500, kept
    expect(hasPending(2)).toBe(true)
    await vi.advanceTimersByTimeAsync(15_000)
    expect(puts).toHaveLength(3)
    expect(puts[2]!.updatedAt).toBeTruthy()
    expect(hasPending(2)).toBe(false)
    view.unmount()
    await saveOnline(2, save) // answered 204 (the stub's last), nothing kept
    expect(puts).toHaveLength(4)
    stub([503])
    await saveOnline(2, save)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(hasPending(2)).toBe(true) // unmounted: no more sends
  })

  it('a profile that changed between keeping a position and the retry: nothing goes out under the new one, the old one’s entry stays for it', async () => {
    const puts = stub([503])
    await saveOnline(2, save) // kept for profile 2
    // profile 3 is the one the page knows now (the cookie is theirs too)
    const view = renderHook(({ id }) => usePendingRetry(id), { initialProps: { id: 3 } })
    await vi.advanceTimersByTimeAsync(60_000)
    expect(puts).toHaveLength(1) // only the failed save itself: nothing under profile 3
    expect(Object.values(kept()).map((x) => x.userId)).toEqual([2])
    // profile 2 is there again: now it goes out, as theirs
    view.rerender({ id: 2 })
    await vi.advanceTimersByTimeAsync(0)
    expect(puts).toHaveLength(2)
    expect(puts[1]!.updatedAt).toBeTruthy()
    expect(kept()).toEqual({})
  })

  it('a send under way when the caller goes (signed out, another profile) is the last: the next kept position is not sent, and stays kept', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const puts: OnlineProgressSave[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_u: RequestInfo | URL, init?: RequestInit) => {
        puts.push(JSON.parse(String(init!.body)) as OnlineProgressSave)
        if (puts.length === 1) await gate // a slow first PUT
        return new Response(null, { status: 204 })
      }),
    )
    const at = new Date().toISOString()
    localStorage.setItem(
      'vault.online.pending',
      JSON.stringify({ a: { userId: 2, savedAt: at, save }, b: { userId: 2, savedAt: at, save: { ...save, tmdbId: 78 } } }),
    )
    const view = renderHook(() => usePendingRetry(2))
    await vi.advanceTimersByTimeAsync(0)
    expect(puts).toHaveLength(1)
    view.unmount() // profile 2 signs out, profile 3 signs in
    release()
    await vi.advanceTimersByTimeAsync(0)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(puts).toHaveLength(1) // the second one never went under the new session
    expect(Object.values(kept()).map((x) => x.userId)).toEqual([2]) // the first is out, the second is kept
  })

  it('flushPending stops before a send once its caller is not current, and while another caller still is it goes on', async () => {
    const puts = stub([])
    const at = new Date().toISOString()
    localStorage.setItem('vault.online.pending', JSON.stringify({ a: { userId: 2, savedAt: at, save } }))
    await flushPending(2, () => false)
    expect(puts).toHaveLength(0)
    expect(hasPending(2)).toBe(true)
    // two callers share one flush: it goes on while either is current
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const sent: number[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_u: RequestInfo | URL, init?: RequestInit) => {
        sent.push((JSON.parse(String(init!.body)) as OnlineProgressSave).tmdbId)
        if (sent.length === 1) await gate
        return new Response(null, { status: 204 })
      }),
    )
    localStorage.setItem('vault.online.pending', JSON.stringify({ a: { userId: 2, savedAt: at, save }, b: { userId: 2, savedAt: at, save: { ...save, tmdbId: 78 } } }))
    let first = true
    const one = flushPending(2, () => first)
    const two = flushPending(2, () => true)
    first = false
    release()
    await Promise.all([one, two])
    expect(sent).toEqual([77, 78])
    expect(hasPending(2)).toBe(false)
  })

  it('nobody known (profile 0): nothing is sent', async () => {
    const puts = stub([503])
    await saveOnline(2, save)
    renderHook(() => usePendingRetry(0))
    await vi.advanceTimersByTimeAsync(60_000)
    expect(puts).toHaveLength(1)
  })

  it('hasPending counts only the profile’s own positions that are not too old', async () => {
    const old = new Date(Date.now() - 2 * 86_400_000).toISOString()
    localStorage.setItem('vault.online.pending', JSON.stringify({ ...entry(2, old), '3': { userId: 3, savedAt: new Date().toISOString(), save } }))
    expect(hasPending(2)).toBe(false) // too old
    expect(hasPending(3)).toBe(true)
    expect(hasPending(4)).toBe(false) // another profile's
    expect(hasPending(0)).toBe(false)
  })

  it("someone else's kept positions are left alone", async () => {
    const puts = stub([])
    localStorage.setItem('vault.online.pending', JSON.stringify(entry(2, new Date().toISOString())))
    await flushPending(3, () => true)
    expect(puts).toHaveLength(0)
    expect(Object.keys(kept())).toHaveLength(1)
    await flushPending(0, () => true) // nobody signed in
    expect(puts).toHaveLength(0)
  })

  it('a position older than a day is dropped, not sent', async () => {
    const puts = stub([])
    localStorage.setItem('vault.online.pending', JSON.stringify(entry(2, new Date(Date.now() - 2 * 86_400_000).toISOString())))
    await flushPending(2, () => true)
    expect(puts).toHaveLength(0)
    expect(kept()).toEqual({})
  })

  it("an old position of another profile goes too, whoever flushes; a fresh one stays", async () => {
    stub([])
    const old = new Date(Date.now() - 2 * 86_400_000).toISOString()
    localStorage.setItem('vault.online.pending', JSON.stringify({ ...entry(2, old), '3': { userId: 3, savedAt: new Date().toISOString(), save } }))
    await flushPending(4, () => true)
    expect(Object.values(kept()).map((x) => x.userId)).toEqual([3])
  })
})
