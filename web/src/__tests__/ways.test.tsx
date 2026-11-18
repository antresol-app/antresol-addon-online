// Whether the way is offered (`useAvailable`): answered from x/available,
// asked early and kept; the title page's warm-up of the sources.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import { session } from '@antresol/test-core/state/session'
import { AVAILABLE_RETRY_MS, AVAILABLE_TTL_MS, prefetchAvailable, refreshAvailable, resetAvailableForTests, useAvailable, WarmSources } from '../ways'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

/** x/available answers from `answers` in turn; the last one repeats. `gate` holds the first answer back. */
function serve(answers: (boolean | 'down')[], gate?: Promise<void>) {
  const urls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      urls.push(url)
      if (!url.endsWith('/x/available')) return json({ sources: [] })
      if (gate && urls.filter((u) => u.endsWith('/x/available')).length === 1) await gate
      const a = answers.length > 1 ? answers.shift()! : answers[0]!
      return a === 'down' ? json({ error: 'нет' }, 500) : json({ available: a })
    }),
  )
  return urls
}
const asked = (urls: string[]) => urls.filter((u) => u.endsWith('/x/available')).length

function Probe({ kind = 'movie', tmdbId = 7, id = 'p' }: { kind?: string; tmdbId?: number; id?: string }) {
  const a = useAvailable({ kind, tmdbId })
  return <output data-testid={id}>{a === null ? 'asking' : a ? 'yes' : 'no'}</output>
}

describe('useAvailable', () => {
  beforeEach(() => {
    resetAvailableForTests()
    vi.useFakeTimers({ toFake: ['Date'] })
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('answers from the prefetched x/available: null while it is on its way, then the answer', async () => {
    let open!: () => void
    const urls = serve([true], new Promise<void>((r) => (open = r)))
    void prefetchAvailable() // the bundle loading
    render(<Probe />)
    expect(screen.getByTestId('p')).toHaveTextContent('asking')
    await act(async () => open())
    expect(screen.getByTestId('p')).toHaveTextContent('yes')
    // one ask for everyone: the prefetch's
    expect(asked(urls)).toBe(1)
  })

  it('a title row mounted later reads the kept answer at once, with no second ask', async () => {
    const urls = serve([true])
    await prefetchAvailable()
    render(
      <>
        <Probe id="a" />
        <Probe id="b" tmdbId={8} />
      </>,
    )
    expect(screen.getByTestId('a')).toHaveTextContent('yes')
    expect(screen.getByTestId('b')).toHaveTextContent('yes')
    expect(asked(urls)).toBe(1)
  })

  it('off or not configured: no', async () => {
    serve([false])
    await prefetchAvailable()
    render(<Probe />)
    expect(screen.getByTestId('p')).toHaveTextContent('no')
  })

  it('a title without a TMDB id, or not a movie or a series, is not offered, without waiting', () => {
    serve([true], new Promise<void>(() => {})) // never answers
    render(
      <>
        <Probe id="none" tmdbId={0} />
        <Probe id="music" kind="album" />
      </>,
    )
    expect(screen.getByTestId('none')).toHaveTextContent('no')
    expect(screen.getByTestId('music')).toHaveTextContent('no')
  })

  it('is kept for a minute, then the next title row asks again and the old answer stays meanwhile', async () => {
    const urls = serve([true, false])
    await prefetchAvailable()
    await prefetchAvailable()
    expect(asked(urls)).toBe(1)
    vi.setSystemTime(Date.now() + AVAILABLE_TTL_MS + 1)
    render(<Probe />)
    // the stale answer is shown while the new one comes: nothing flickers
    expect(screen.getByTestId('p')).toHaveTextContent('yes')
    await act(async () => void (await prefetchAvailable()))
    expect(asked(urls)).toBe(2)
    expect(screen.getByTestId('p')).toHaveTextContent('no')
  })

  it('a failed ask is not offered (none known yet) or keeps the last answer, and is asked again after ten seconds', async () => {
    const urls = serve(['down', true])
    await prefetchAvailable()
    render(<Probe />)
    expect(screen.getByTestId('p')).toHaveTextContent('no')
    await prefetchAvailable()
    expect(asked(urls)).toBe(1) // too soon
    vi.setSystemTime(Date.now() + AVAILABLE_RETRY_MS + 1)
    await act(async () => void (await prefetchAvailable()))
    expect(asked(urls)).toBe(2)
    expect(screen.getByTestId('p')).toHaveTextContent('yes')
    // a later failure leaves the answer as it was
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'нет' }, 500)))
    vi.setSystemTime(Date.now() + AVAILABLE_TTL_MS + 1)
    await act(async () => void (await prefetchAvailable()))
    expect(screen.getByTestId('p')).toHaveTextContent('yes')
  })

  it('refreshAvailable asks now (the admin changed the setting)', async () => {
    const urls = serve([true, false])
    await prefetchAvailable()
    render(<Probe />)
    expect(screen.getByTestId('p')).toHaveTextContent('yes')
    await act(async () => void (await refreshAvailable()))
    expect(asked(urls)).toBe(2)
    expect(screen.getByTestId('p')).toHaveTextContent('no')
  })
})

describe('refreshAvailable while an ask is on its way', () => {
  beforeEach(() => resetAvailableForTests())
  afterEach(() => vi.unstubAllGlobals())

  it('does not take the answer sent before the change for the new one: it asks again once that one is in', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let n = 0
    const urls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        urls.push(String(input))
        n++
        if (n === 1) await gate // the ask sent before the toggle: still «on»
        return json({ available: n === 1 })
      }),
    )
    render(<Probe />)
    const first = prefetchAvailable()
    const second = refreshAvailable() // the admin switched it off meanwhile
    release()
    await act(async () => void (await Promise.all([first, second])))
    expect(urls).toHaveLength(2)
    expect(screen.getByTestId('p')).toHaveTextContent('no')
    // and that answer is the kept one, not the stale «on»
    await prefetchAvailable()
    expect(urls).toHaveLength(2)
  })
})

describe('WarmSources', () => {
  beforeEach(() => resetAvailableForTests())
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })
  const title = { id: 0, kind: 'movie', tmdbId: 7, title: 'Дюна' } as never
  const sourcesAsked = (urls: string[]) => urls.filter((u) => u.includes('/x/sources')).length

  it('asks the sources of a title off the library once its page has been read for a moment, and draws nothing', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    const urls = serve([true])
    await prefetchAvailable()
    const { container } = render(<WarmSources item={title} />)
    expect(container).toBeEmptyDOMElement()
    await act(async () => void (await vi.advanceTimersByTimeAsync(700)))
    expect(sourcesAsked(urls)).toBe(0) // a quick pass-through does not ask
    await act(async () => void (await vi.advanceTimersByTimeAsync(200)))
    expect(sourcesAsked(urls)).toBe(1)
    expect(urls.find((u) => u.includes('/x/sources'))).toBe('/api/addons/online/x/sources?kind=movie&tmdbId=7')
  })

  it('leaving the page first asks nothing; a library title, or a way that is off, asks nothing', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    const urls = serve([true])
    await prefetchAvailable()
    render(<WarmSources item={title} />).unmount()
    render(<WarmSources item={{ ...title, id: 5 }} />)
    await act(async () => void (await vi.advanceTimersByTimeAsync(2000)))
    expect(sourcesAsked(urls)).toBe(0)
    resetAvailableForTests()
    serve([false])
    await prefetchAvailable()
    render(<WarmSources item={title} />)
    await act(async () => void (await vi.advanceTimersByTimeAsync(2000)))
    expect(sourcesAsked(urls)).toBe(0)
  })

  it('sends the online positions kept for the profile it knows, and none for another', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] })
    const puts: unknown[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        if (url.endsWith('/api/login')) return json({ id: 7, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null })
        if (url.endsWith('/x/progress') && init?.method === 'PUT') {
          puts.push(JSON.parse(String(init.body)))
          return new Response(null, { status: 204 })
        }
        return json({ available: false })
      }),
    )
    const save = { kind: 'movie', tmdbId: 77, season: 0, episode: 0, positionMs: 60_000, durationMs: 600_000, source: 'a', voice: '', quality: '' }
    const at = new Date().toISOString()
    localStorage.setItem('vault.online.pending', JSON.stringify({ mine: { userId: 7, savedAt: at, save }, theirs: { userId: 8, savedAt: at, save: { ...save, tmdbId: 78 } } }))
    session.reset()
    await session.login(7)
    try {
      render(<WarmSources item={{ id: 5, kind: 'movie', tmdbId: 77, title: 'x' } as never} />)
      await act(async () => void (await vi.advanceTimersByTimeAsync(0)))
      expect(puts).toEqual([expect.objectContaining({ tmdbId: 77, updatedAt: at })])
      expect(Object.keys(JSON.parse(localStorage.getItem('vault.online.pending')!))).toEqual(['theirs'])
    } finally {
      session.reset()
      localStorage.clear()
    }
  })
})
