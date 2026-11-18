// The addon's registration: the way, the episode guide's online sources, the
// full-window player route, the admin's pages and their tile, and the strings
// every label and title names.
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'
// the app's slot registry (a test-only alias of web/vitest.online.config.ts)
import { contributionsOf, resetAddonsForTests } from '@antresol/test-registry'

const calls: string[] = []

describe('registerAddon("online")', () => {
  // the bundle registers once, when its module first loads
  beforeAll(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        calls.push(String(input))
        return new Response(JSON.stringify({ available: true }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }),
    )
  })
  afterAll(() => {
    resetAddonsForTests()
    vi.unstubAllGlobals()
  })

  it('registers the way, the episode action, the full-window route', async () => {
    await import('../index')
    const c = contributionsOf('online')
    expect(c).toBeDefined()
    // the player is over the whole window; the two pages are the admin's
    expect(c?.routes?.map((r) => [r.path, Boolean(r.fullWindow), Boolean(r.admin)])).toEqual([
      ['play/:kind/:tmdbId', true, false],
      ['settings', false, true],
      ['status', false, true],
    ])
    expect(c?.manageTiles?.map((t) => [t.to, t.icon, t.tint, t.group, t.admin])).toEqual([['settings', 'globe', 'teal', 'downloads', true]])
    // the way: a «Смотреть ▾» entry (id online:way) that asks `useAvailable` and runs the picker
    expect(c?.itemActions).toHaveLength(1)
    const way = c!.itemActions![0]!
    expect([way.label, way.icon, way.watchWay, way.admin]).toEqual(['way', 'play', true, undefined])
    expect(typeof way.useAvailable).toBe('function')
    expect(typeof way.run).toBe('function')
    // the guide's online sources: the registry's real shape (labels and order are maps by kind)
    expect(c?.episodeActions).toHaveLength(1)
    const ea = c!.episodeActions![0]!
    expect(ea.kinds).toEqual(['online'])
    expect(ea.labels).toEqual({ online: 'kind' })
    expect(ea.order).toEqual({ online: 20 })
    expect(typeof ea.use).toBe('function')
    expect(ea.admin).toBeUndefined()
    // a panel that draws nothing and warms the title page's sources
    expect(c?.itemPanels).toHaveLength(1)
    expect(c?.itemPanels?.[0]?.admin).toBeFalsy()
  })

  it('every label and title is a strings key with Russian and English words', async () => {
    await import('../index')
    const c = contributionsOf('online')!
    const keys = [
      ...(c.routes ?? []).map((r) => r.title),
      ...(c.manageTiles ?? []).map((t) => t.label),
      ...(c.itemActions ?? []).map((a) => a.label),
      ...(c.episodeActions ?? []).flatMap((a) => Object.values(a.labels ?? {})),
    ]
    expect(keys).toEqual(['common.watchOnline', 'settings.title', 'status.title', 'settings.title', 'way', 'kind'])
    for (const key of keys) {
      expect(c.strings?.ru?.[key], key).toBeTruthy()
      expect(c.strings?.en?.[key], key).toBeTruthy()
    }
    expect(c.strings?.ru?.way).toBe('Онлайн')
    expect(c.strings?.en?.way).toBe('Online')
    expect(c.strings?.ru?.kind).toBe('Онлайн')
    expect(c.strings?.en?.['common.watchOnline']).toBe('Watch Online')
    // the two languages have the same keys
    expect(Object.keys(c.strings?.en ?? {}).sort()).toEqual(Object.keys(c.strings?.ru ?? {}).sort())
  })

  it('the way’s run returns the picker for a movie or a series with a TMDB id, nothing else', async () => {
    await import('../index')
    const way = contributionsOf('online')!.itemActions![0]!
    const close = () => {}
    const movie = { id: 0, kind: 'movie', tmdbId: 42, title: 'Дюна', poster: '/p.jpg' } as never
    expect(way.run(movie, close)).toMatchObject({ props: { kind: 'movie', tmdbId: 42, title: 'Дюна', poster: '/p.jpg', onClose: close } })
    expect(way.run({ id: 5, kind: 'series', tmdbId: 9, title: 'Шоу', poster: '' } as never, close)).toMatchObject({ props: { kind: 'series', tmdbId: 9, poster: undefined } })
    expect(way.run({ id: 5, kind: 'music', tmdbId: 9, title: 'x', poster: '' } as never, close)).toBeUndefined()
    expect(way.run({ id: 5, kind: 'movie', title: 'x', poster: '' } as never, close)).toBeUndefined()
  })

  it('asks x/available as soon as the bundle loads', async () => {
    await import('../index')
    expect(calls.filter((u) => u === '/api/addons/online/x/available')).toHaveLength(1)
  })
})
