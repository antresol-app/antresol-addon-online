import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import type { EpisodePlay, TitleEpisode } from '@antresol/addon-sdk'
import { ep, online } from '@antresol/test-core/components/episodes/__tests__/fixtures'
import { session } from '@antresol/test-core/state/session'
import { writeWatchWay } from '@antresol/test-core/state/watchWay'
import { useOnlineEpisodePlay } from '../episodes'
import { prefetchAvailable, resetAvailableForTests } from '../ways'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const adult = { id: 2, name: 'Test', avatar: 'grad-1', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
const kid = { ...adult, id: 3, isKid: true }

/** The server: the way is on (`available`), the picker finds no sources; every other call answers the profile. */
function serve(user: Record<string, unknown> = adult, available = true) {
  const f = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.endsWith('/x/available')) return json({ available })
    if (url.includes('/x/sources')) return json({ sources: [] })
    if (url.includes('/x/progress')) return json({ entries: [] })
    return json(user)
  })
  vi.stubGlobal('fetch', f)
  return f
}

const Where = () => {
  const l = useLocation()
  return <output data-testid="where" data-state={JSON.stringify(l.state ?? null)}>{l.pathname}</output>
}
let hook: EpisodePlay | null = null
function Harness({ episodes, kind = 'series' }: { episodes: TitleEpisode[]; kind?: 'series' | 'movie' }) {
  const p = useOnlineEpisodePlay({ kind, tmdbId: 5, title: 'Show', episodes })
  hook = p
  return <>{p?.node}</>
}
const mount = (episodes: TitleEpisode[]) =>
  render(
    <MemoryRouter>
      <Harness episodes={episodes} />
      <Where />
    </MemoryRouter>,
  )
const where = () => screen.getByTestId('where')
const signIn = async (user: Record<string, unknown>) => {
  session.reset()
  const f = serve(user)
  await session.login(user.id as number)
  await prefetchAvailable()
  return f
}

describe('useOnlineEpisodePlay', () => {
  beforeEach(() => {
    localStorage.clear()
    resetAvailableForTests()
    hook = null
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    session.reset()
  })

  it('online source opens the online player with provider, voice and the episode list', async () => {
    await signIn(adult)
    const eps = [
      ep(1, { sources: [online('alpha', 'Voice A', { ref: 'r1' }), online('beta', 'Voice A', { ref: 'b1' })] }),
      ep(2, { sources: [online('alpha', 'Other dub', { ref: 'other-voice' })] }),
      ep(3, { sources: [online('alpha', 'Voice A', { ref: 'r3' })] }),
    ]
    mount(eps)
    act(() => hook!.play(eps[0]!, eps[0]!.sources[0]!))
    await waitFor(() => expect(where()).toHaveTextContent('/addons/online/play/series/5'))
    const state = JSON.parse(where().dataset.state!)
    expect(state).toMatchObject({ ref: 'r1', quality: '', title: 'Show', season: 1, episode: 1, source: 'alpha', voice: 'Voice A' })
    expect(state.episodes).toEqual([
      { season: 1, episode: 1, name: 'Name 1', ref: 'r1' },
      { season: 1, episode: 3, name: 'Name 3', ref: 'r3' },
    ])
  })

  it('a source of another kind is not its to play', async () => {
    await signIn(adult)
    const e = ep(1, { sources: [online('alpha', 'Voice A', { kind: 'pinned' })] })
    mount([e])
    act(() => hook!.play(e, e.sources[0]!))
    expect(where()).toHaveTextContent(/^\/$/)
    expect(where().dataset.state).toBe('null')
  })

  it('other sources opens the online picker on that episode when the way is offered', async () => {
    const f = await signIn(adult)
    const e = ep(2, { sources: [online('alpha', 'Voice A', { ref: 'r2' })] })
    mount([e])
    act(() => hook!.other(e))
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/sources'))).toBe(true))
    expect(await screen.findByRole('dialog', { name: 'Смотреть онлайн' })).toBeInTheDocument()
  })

  it('offers nothing while the way is off, and nothing before the first answer', async () => {
    session.reset()
    serve(adult, false)
    await session.login(2)
    await prefetchAvailable()
    mount([ep(2, { sources: [online('alpha', 'Voice A', { ref: 'r2' })] })])
    expect(hook).toBeNull()
    // not answered yet: not offered either (the way appears when it answers true)
    resetAvailableForTests()
    serve(adult, true)
    hook = null
    mount([ep(2, { sources: [online('alpha', 'Voice A', { ref: 'r2' })] })])
    expect(hook).toBeNull()
    await waitFor(() => expect(hook).not.toBeNull())
  })

  it('a kid gets none', async () => {
    await signIn(kid)
    mount([ep(1, { sources: [online('alpha', 'Voice A')] })])
    expect(hook).toBeNull()
  })

  it('prefers online sources once the profile chose the online way last', async () => {
    await signIn(adult)
    mount([ep(1, { sources: [online('alpha', 'Voice A')] })])
    expect(hook?.prefer).toBeUndefined()
    act(() => writeWatchWay(2, 'online:way'))
    await waitFor(() => expect(hook?.prefer).toEqual(['online']))
    act(() => writeWatchWay(2, 'torrents:watch.now'))
    await waitFor(() => expect(hook?.prefer).toBeUndefined())
  })
})
