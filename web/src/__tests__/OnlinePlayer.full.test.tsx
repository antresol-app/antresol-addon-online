import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import { ApiError } from '@antresol/addon-sdk'
import type { OnlinePlayState } from '../OnlineSheet'

// the core's heartbeat and HLS stop: an online stream must not touch them
vi.mock('@antresol/test-core/api/client', async (orig) => ({
  ...(await orig<typeof import('@antresol/test-core/api/client')>()),
  touchHls: vi.fn(() => new Promise(() => {})),
  stopHls: vi.fn(() => Promise.resolve()),
}))
vi.mock('../api', async (orig) => ({
  ...(await orig<typeof import('../api')>()),
  onlinePlay: vi.fn(),
  onlineStop: vi.fn(() => Promise.resolve()),
  onlineProgress: vi.fn(),
  saveOnlineProgress: vi.fn(() => Promise.resolve()),
}))
vi.mock('hls.js', () => {
  class FakeHls {
    static Events = { ERROR: 'error' }
    static ErrorTypes = { NETWORK_ERROR: 'networkError' }
    static isSupported = () => true
    on() {}
    loadSource() {}
    attachMedia() {}
    destroy() {}
  }
  return { default: FakeHls }
})

import { touchHls, stopHls } from '@antresol/test-core/api/client'
import { audio, resetAudioForTests } from '@antresol/test-core/player/audio/engine'
import { session } from '@antresol/test-core/state/session'
import { onlinePlay, onlineProgress, onlineStop, saveOnlineProgress } from '../api'
import { OnlinePlayer } from '../OnlinePlayer'

const PLAY = '/addons/online/play/:kind/:tmdbId'
const MEDIA = '/api/addons/online/x/media/'

const episodes = [1, 2, 3].map((e) => ({ season: 2, episode: e, name: `Серия ${e}`, ref: `ep-2-${e}` }))
const state: OnlinePlayState = {
  ref: 'ep-2-2',
  quality: '720p',
  title: 'Сериал',
  subtitle: 'Сезон 2, серия 2',
  season: 2,
  episode: 2,
  source: 'alpha',
  voice: 'Дубляж',
  episodes,
}

let n = 0
beforeEach(() => {
  n = 0
  resetAudioForTests()
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockImplementation(() => Promise.resolve())
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {})
  vi.spyOn(HTMLMediaElement.prototype, 'load').mockImplementation(() => {})
  vi.mocked(onlinePlay).mockImplementation(async (body) => {
    n++
    return {
      mode: 'stream', kind: 'mp4', url: `${MEDIA}sid${n}/${body.ref}`, session: `sid${n}`, startSec: body.startSec ?? 0,
      audio: [], audioIndex: -1, subtitles: [], quality: { options: ['1080p', '720p'], current: body.quality || '1080p' },
    }
  })
  vi.mocked(saveOnlineProgress).mockImplementation(() => Promise.resolve())
  vi.mocked(onlineProgress).mockResolvedValue({
    entries: [{ kind: 'series', tmdbId: 77, season: 2, episode: 2, positionMs: 30_000, durationMs: 600_000, finished: false, source: 'alpha', voice: 'Дубляж', quality: '720p', updatedAt: '' }],
    last: { source: 'alpha', voice: 'Дубляж', quality: '720p', season: 2, episode: 2 },
  })
})
afterEach(() => {
  vi.clearAllMocks()
  vi.unstubAllGlobals()
  resetAudioForTests()
  session.reset()
  localStorage.clear()
})

function renderPlayer() {
  return render(
    <MemoryRouter initialEntries={[{ pathname: '/addons/online/play/series/77', state }]}>
      <Routes>
        <Route path={PLAY} element={<OnlinePlayer />} />
        <Route path="/title/:kind/:tmdbId" element={<p>Страница</p>} />
      </Routes>
    </MemoryRouter>,
  )
}

/** The element got its metadata: the online stream knows its length. */
async function loaded() {
  const video = (await screen.findByTestId('player')).querySelector('video')!
  await waitFor(() => expect(video.getAttribute('src')).toMatch(/^\/api\/addons\/online\/x\/media\//))
  Object.defineProperty(video, 'duration', { configurable: true, value: 600 })
  act(() => {
    fireEvent.loadedMetadata(video)
  })
  return video
}

describe('OnlinePlayer', () => {
  it('renders ui.Player with stream playback: the addon’s proxy path, its session, no library progress and no HLS heartbeat', async () => {
    renderPlayer()
    const video = await loaded()
    expect(video.getAttribute('src')).toBe(`${MEDIA}sid1/ep-2-2`)
    // the play request is the addon's own call, at the saved place and quality
    expect(onlinePlay).toHaveBeenCalledTimes(1)
    expect(onlinePlay).toHaveBeenCalledWith(expect.objectContaining({ ref: 'ep-2-2', quality: '720p', startSec: 30, caps: expect.anything() }))
    // «Другой источник» is the stream's top action
    expect(screen.getByRole('button', { name: 'Другой источник' })).toBeInTheDocument()
    expect(touchHls).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Закрыть плеер' }))
    // the stream's stop is the addon's call (never the core's HLS stop)
    await waitFor(() => expect(onlineStop).toHaveBeenCalledWith('sid1', false))
    expect(stopHls).not.toHaveBeenCalled()
  })

  it('leaving without close (the route changes, the tab closes) sends the last position and the stop with keepalive', async () => {
    const view = renderPlayer()
    await loaded()
    view.unmount()
    await waitFor(() => expect(onlineStop).toHaveBeenCalledWith('sid1', true))
    expect(vi.mocked(saveOnlineProgress).mock.calls.at(-1)).toEqual([expect.objectContaining({ tmdbId: 77, season: 2, episode: 2, positionMs: 30_000 }), true])
  })

  it('pagehide sends the position and the stop with keepalive, the page staying as it is', async () => {
    renderPlayer()
    await loaded()
    act(() => {
      window.dispatchEvent(new Event('pagehide'))
    })
    await waitFor(() => expect(onlineStop).toHaveBeenCalledWith('sid1', true))
    expect(vi.mocked(saveOnlineProgress).mock.calls.at(-1)![1]).toBe(true)
    expect(screen.getByTestId('player')).toBeInTheDocument()
  })

  it('a signed-in profile whose save gets a 503 keeps the position under the id useUser() gives', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).endsWith('/api/login')
          ? new Response(JSON.stringify({ id: 7, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }), { status: 200, headers: { 'Content-Type': 'application/json' } })
          : new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
      ),
    )
    await session.login(7)
    vi.mocked(saveOnlineProgress).mockImplementation(() => Promise.reject(new ApiError(503, 'x')))
    renderPlayer()
    await loaded()
    await userEvent.click(screen.getByRole('button', { name: 'Закрыть плеер' }))
    await waitFor(() => expect(Object.values(JSON.parse(localStorage.getItem('vault.online.pending') ?? '{}'))).toHaveLength(1))
    expect(Object.values(JSON.parse(localStorage.getItem('vault.online.pending')!))).toEqual([expect.objectContaining({ userId: 7, save: expect.objectContaining({ tmdbId: 77, episode: 2 }) })])
  })

  it('a kept position being sent when the player goes (sign-out, another profile) is the last: the next one is not sent and stays kept', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).endsWith('/api/login')
          ? new Response(JSON.stringify({ id: 7, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }), { status: 200, headers: { 'Content-Type': 'application/json' } })
          : new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
      ),
    )
    await session.login(7)
    const at = new Date().toISOString()
    const kept = { kind: 'series', tmdbId: 77, season: 1, episode: 1, positionMs: 5_000, durationMs: 600_000, source: 'alpha', voice: '', quality: '' }
    localStorage.setItem('vault.online.pending', JSON.stringify({ a: { userId: 7, savedAt: at, save: kept }, b: { userId: 7, savedAt: at, save: { ...kept, episode: 2 } } }))
    let release!: () => void
    vi.mocked(saveOnlineProgress).mockImplementationOnce(() => new Promise<void>((r) => (release = r))) // a slow first send
    const view = renderPlayer()
    await waitFor(() => expect(saveOnlineProgress).toHaveBeenCalledTimes(1))
    view.unmount()
    release()
    await waitFor(() => expect(Object.keys(JSON.parse(localStorage.getItem('vault.online.pending') ?? '{}'))).toEqual(['b']))
    await new Promise((r) => setTimeout(r, 30))
    expect(saveOnlineProgress).toHaveBeenCalledTimes(1)
  })

  it('yields the music session before playing', async () => {
    const yielded = vi.spyOn(audio, 'yieldSession')
    renderPlayer()
    await loaded()
    expect(yielded).toHaveBeenCalled()
    expect(Math.min(...yielded.mock.invocationCallOrder)).toBeLessThan(Math.min(...vi.mocked(onlinePlay).mock.invocationCallOrder))
  })

  it('a card without a state whose source is gone opens the title page with the online way', async () => {
    vi.mocked(onlineProgress).mockResolvedValue({ entries: [], last: null })
    const Where = () => {
      const l = useLocation()
      return <output data-testid="where">{l.pathname + l.search}</output>
    }
    render(
      <MemoryRouter initialEntries={['/addons/online/play/series/77?s=1&e=2&t=90']}>
        <Routes>
          <Route path={PLAY} element={<OnlinePlayer />} />
          <Route path="/title/:kind/:tmdbId" element={<Where />} />
        </Routes>
      </MemoryRouter>,
    )
    // nothing remembered: the title page runs the way once (`?way=<addon>:<label>`)
    expect(await screen.findByTestId('where')).toHaveTextContent('/title/series/77?way=online:way')
    expect(onlinePlay).not.toHaveBeenCalled()
  })

  it('«Другой источник» opens the picker inside the player, without leaving full screen', async () => {
    const exit = vi.fn(() => Promise.resolve())
    Object.defineProperty(document, 'exitFullscreen', { configurable: true, value: exit })
    renderPlayer()
    await loaded()
    await userEvent.click(screen.getByRole('button', { name: 'Другой источник' }))
    const dialog = await screen.findByRole('dialog', { name: 'Смотреть онлайн' })
    expect(screen.getByTestId('player').contains(dialog)).toBe(true)
    expect(exit).not.toHaveBeenCalled()
  })

  it('starts at the saved position with the chosen quality', async () => {
    renderPlayer()
    await loaded()
    expect(onlinePlay).toHaveBeenCalledWith(expect.objectContaining({ ref: 'ep-2-2', quality: '720p', startSec: 30 }))
  })

  it('saves the progress with source, voice, quality, season and episode; close stops the session', async () => {
    renderPlayer()
    await loaded()
    await userEvent.click(screen.getByRole('button', { name: 'Закрыть плеер' }))
    await waitFor(() => expect(saveOnlineProgress).toHaveBeenCalled())
    expect(vi.mocked(saveOnlineProgress).mock.calls.at(-1)![0]).toEqual({
      kind: 'series',
      tmdbId: 77,
      season: 2,
      episode: 2,
      positionMs: 30_000,
      durationMs: 600_000,
      source: 'alpha',
      voice: 'Дубляж',
      quality: '720p',
      nextSeason: 2,
      nextEpisode: 3,
    })
    await waitFor(() => expect(onlineStop).toHaveBeenCalledWith('sid1', false))
  })

  it('«Следующая серия» starts the next ref of the same voice', async () => {
    renderPlayer()
    const video = await loaded()
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 599.5 })
    act(() => {
      fireEvent.ended(video)
    })
    await waitFor(() => expect(onlinePlay).toHaveBeenCalledWith(expect.objectContaining({ ref: 'ep-2-3', quality: '720p' })))
    // the finished episode was saved as finished, with its next one
    expect(saveOnlineProgress).toHaveBeenCalledWith(expect.objectContaining({ episode: 2, positionMs: 600_000, nextEpisode: 3 }), false)
  })

  it('the last listed episode says so', async () => {
    vi.mocked(onlineProgress).mockResolvedValue({ entries: [], last: null })
    render(
      <MemoryRouter initialEntries={[{ pathname: '/addons/online/play/series/77', state: { ...state, ref: 'ep-2-3', episode: 3 } }]}>
        <Routes>
          <Route path={PLAY} element={<OnlinePlayer />} />
          <Route path="/title/:kind/:tmdbId" element={<p>Страница</p>} />
        </Routes>
      </MemoryRouter>,
    )
    const video = await loaded()
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 120 })
    act(() => {
      fireEvent.timeUpdate(video)
    })
    await userEvent.click(screen.getByRole('button', { name: 'Закрыть плеер' }))
    await waitFor(() => expect(saveOnlineProgress).toHaveBeenCalledWith(expect.objectContaining({ episode: 3, lastListed: true }), false))
  })

  it('the subtitle menu lists the source’s tracks by language and label, and turns one on', async () => {
    const vtt = vi.fn(async () => new Response('WEBVTT\n\n00:00:01.000 --> 00:00:05.000\nПривет\n', { status: 200 }))
    vi.stubGlobal('fetch', vtt)
    vi.mocked(onlinePlay).mockImplementation(async (body) => ({
      mode: 'stream', kind: 'mp4', url: `${MEDIA}sid1/${body.ref}`, session: 'sid1', startSec: 0,
      audio: [], audioIndex: -1, quality: { options: ['1080p'], current: '1080p' },
      subtitles: [
        { index: 0, codec: 'webvtt', lang: 'ru', title: 'Русские (Forced)', forced: false, default: false, external: true, burnIn: false, url: '/api/addons/online/x/media/sid1/subs/0.vtt' },
        { index: 1, codec: 'webvtt', lang: 'en', title: 'English', forced: false, default: false, external: true, burnIn: false, url: '/api/addons/online/x/media/sid1/subs/1.vtt' },
        // an older server's shape: «label», no title
        { index: 2, lang: '', label: 'Надписи', url: '/api/addons/online/x/media/sid1/subs/2.vtt' } as never,
      ],
    }))
    try {
      renderPlayer()
      await loaded()
      await userEvent.click(screen.getByRole('button', { name: 'Аудио и субтитры' }))
      const group = await screen.findByRole('group', { name: 'Субтитры' })
      const items = within(group).getAllByRole('menuitemradio')
      expect(items.map((x) => x.textContent)).toEqual(['Выключены', 'РусскийРусские (Forced)', 'Английский', 'Субтитры 3'])
      await userEvent.click(items[1]!)
      await waitFor(() => expect(vtt).toHaveBeenCalledWith('/api/addons/online/x/media/sid1/subs/0.vtt', expect.anything()))
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
