// The addon's calls: every one goes to its own server through the core's
// proxy (/api/addons/online/x/…); what must outlive the page (a position
// saved on close, the stream's stop) keeps the `keepalive` flag.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@antresol/addon-sdk'
import {
  onlineAvailable,
  onlineBrowse,
  onlinePlay,
  onlineProgress,
  onlineSettings,
  onlineSource,
  onlineSources,
  onlineStatus,
  onlineStop,
  saveOnlineProgress,
  saveOnlineSettings,
} from '../api'

const X = '/api/addons/online/x/'
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

type Seen = { url: string; init: RequestInit }
function serve(answer: (url: string, init: RequestInit) => Response = () => json({})) {
  const seen: Seen[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      seen.push({ url: String(input), init })
      return answer(String(input), init)
    }),
  )
  return seen
}
afterEach(() => vi.unstubAllGlobals())

describe('the addon’s calls', () => {
  it('read the sources, a source, a level and the progress under x/', async () => {
    const seen = serve((u) =>
      u.includes('/sources') ? json({ sources: [{ id: 'a', name: 'A', quality: '', ref: 'r' }] }) : u.includes('/source?') ? json({ source: { id: 'a', name: 'A', quality: '', ref: 'r' } }) : json({ entries: [], last: null }),
    )
    expect(await onlineSources('movie', 7)).toHaveLength(1)
    await onlineSource('series', 7, 'a')
    await onlineBrowse('some ref/with:chars')
    await onlineProgress('movie', 7)
    expect(seen.map((s) => s.url)).toEqual([
      `${X}sources?kind=movie&tmdbId=7`,
      `${X}source?kind=series&tmdbId=7&source=a`,
      `${X}browse?ref=some+ref%2Fwith%3Achars`,
      `${X}progress?kind=movie&tmdbId=7`,
    ])
    expect(seen.every((s) => (s.init.method ?? 'GET') === 'GET')).toBe(true)
  })

  it('play posts the request and answers the stream’s play answer', async () => {
    const seen = serve(() => json({ mode: 'stream', kind: 'hls', url: `${X}media/s1/index.m3u8`, session: 's1', startSec: 0, audio: [], audioIndex: -1, subtitles: [] }))
    const r = await onlinePlay({ ref: 'f1', quality: '720p', caps: { video: [], audio: [], containers: [], maxHeight: 1080 } as never })
    expect(r.url).toBe(`${X}media/s1/index.m3u8`)
    expect(seen[0]!.url).toBe(`${X}play`)
    expect(seen[0]!.init.method).toBe('POST')
    expect(JSON.parse(String(seen[0]!.init.body))).toMatchObject({ ref: 'f1', quality: '720p' })
  })

  it('stopping a session is a DELETE of its media path; saving a position is a PUT', async () => {
    const seen = serve(() => new Response(null, { status: 204 }))
    await onlineStop('sid-1')
    await saveOnlineProgress({ kind: 'movie', tmdbId: 7, season: 0, episode: 0, positionMs: 1, durationMs: 2, source: 'a', voice: '', quality: '' })
    expect(seen.map((s) => [s.init.method, s.url])).toEqual([
      ['DELETE', `${X}media/sid-1`],
      ['PUT', `${X}progress`],
    ])
    expect(seen.every((s) => !s.init.keepalive)).toBe(true)
  })

  it('a call that leaves with the page is sent with keepalive, to the same path, as JSON', async () => {
    const seen = serve(() => new Response(null, { status: 204 }))
    await onlineStop('s1', true)
    await saveOnlineProgress({ kind: 'movie', tmdbId: 7, season: 0, episode: 0, positionMs: 1, durationMs: 2, source: 'a', voice: '', quality: '' }, true)
    expect(seen.map((s) => [s.init.method, s.url, s.init.keepalive])).toEqual([
      ['DELETE', `${X}media/s1`, true],
      ['PUT', `${X}progress`, true],
    ])
    for (const s of seen) {
      expect((s.init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
      expect(s.init.credentials).toBe('same-origin')
    }
    expect(JSON.parse(String(seen[1]!.init.body))).toMatchObject({ positionMs: 1 })
  })

  it('a refused keepalive call is an ApiError with the server’s status; a lost one has status 0', async () => {
    serve(() => json({ error: 'нет' }, 503))
    await expect(saveOnlineProgress({ kind: 'movie', tmdbId: 7, season: 0, episode: 0, positionMs: 1, durationMs: 2, source: 'a', voice: '', quality: '' }, true)).rejects.toMatchObject({ status: 503, message: 'нет' })
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('net'))))
    const e = await onlineStop('s1', true).catch((x: unknown) => x)
    expect(e).toBeInstanceOf(ApiError)
    expect(e).toMatchObject({ status: 0 })
  })

  it('the admin’s status, the setting and its save, and the availability', async () => {
    const seen = serve((u) => (u.endsWith('available') ? json({ available: true }) : u.endsWith('admin/status') ? json({ configured: true, reachable: true, sources: 4, version: '1' }) : json({ enabled: false })))
    expect(await onlineAvailable()).toBe(true)
    expect((await onlineStatus()).sources).toBe(4)
    expect(await onlineSettings()).toEqual({ enabled: false })
    await saveOnlineSettings(true)
    expect(seen.map((s) => [s.init.method ?? 'GET', s.url])).toEqual([
      ['GET', `${X}available`],
      ['GET', `${X}admin/status`],
      ['GET', `${X}admin/settings`],
      ['PUT', `${X}admin/settings`],
    ])
    expect(JSON.parse(String(seen[3]!.init.body))).toEqual({ enabled: true })
  })
})
