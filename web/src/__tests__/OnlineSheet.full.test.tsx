import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import { session } from '@antresol/test-core/state/session'
import { offerBadges, offerOf, OnlineSheet } from '../OnlineSheet'

const adult = { id: 2, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

type Routes = Record<string, () => Response>

/** fetch stub: x/* answers from `routes` (by path + ref), everything else {}. */
function stub(routes: Routes, user = adult) {
  const f = vi.fn(async (input: RequestInfo | URL) => {
    const u = new URL(String(input), 'http://x')
    if (u.pathname === '/api/login') return json(user)
    const key = u.pathname === '/api/addons/online/x/browse' ? `browse:${u.searchParams.get('ref')}` : u.pathname
    const r = routes[key]
    if (r) return r()
    if (u.pathname === '/api/addons/online/x/progress') return json({ entries: [], last: null })
    return json({})
  })
  vi.stubGlobal('fetch', f)
  return f
}

const src = (id: string, name: string) => ({ id, name, quality: '1080p', ref: `src-${id}` })
const eps = (season: number) =>
  [1, 2, 3].map((e) => ({ season, episode: e, name: `Серия ${e}`, ref: `ep-${season}-${e}`, qualities: ['1080p', '720p'], subtitles: 0 }))
const episodesLevel = (season: number) => ({
  level: 'episodes',
  season,
  voices: [{ name: 'Дубляж', active: true, ref: `v-dub-${season}` }],
  episodes: eps(season),
})

let seen: { pathname: string; state: unknown } | null = null
function Spy() {
  const l = useLocation()
  seen = { pathname: l.pathname, state: l.state }
  return null
}

function renderSheet(kind: 'movie' | 'series' = 'series') {
  const onClose = vi.fn()
  render(
    <MemoryRouter initialEntries={['/title/x']}>
      <Routes>
        <Route path="*" element={<><Spy /><OnlineSheet kind={kind} tmdbId={77} title="Дюна" onClose={onClose} /></>} />
      </Routes>
    </MemoryRouter>,
  )
  return onClose
}

const groups = () => screen.getAllByRole('radiogroup').map((g) => g.getAttribute('aria-label'))

describe('OnlineSheet', () => {
  beforeEach(async () => {
    session.reset()
    seen = null
  })
  afterEach(() => vi.unstubAllGlobals())

  it('one source and one voice: only «Сезон», «Серия» and «Качество» are lists', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      'browse:src-alpha': () =>
        json({ level: 'seasons', seasons: [{ number: 1, name: '', ref: 's1' }, { number: 2, name: '', ref: 's2' }] }),
      'browse:s1': () => json(episodesLevel(1)),
      'browse:s2': () => json(episodesLevel(2)),
    })
    await session.login(2)
    renderSheet()
    expect(screen.getByText('Ищем источники…')).toBeInTheDocument()
    await screen.findByRole('radiogroup', { name: 'Серия' })
    expect(groups()).toEqual(['Сезон', 'Серия', 'Качество'])
    expect(screen.getByTestId('summary-Источник')).toHaveTextContent('Alpha')
    expect(screen.getByTestId('summary-Озвучка')).toHaveTextContent('Дубляж')

    // season 2 → episode 3 → «Смотреть» opens the player with the episode list
    await userEvent.click(screen.getByRole('radio', { name: 'Сезон 2' }))
    await waitFor(() => expect(screen.getByRole('radio', { name: 'Сезон 2' })).toHaveAttribute('aria-checked', 'true'))
    await userEvent.click(await screen.findByRole('radio', { name: /Серия 3/ }))
    await userEvent.click(screen.getByRole('radio', { name: '720p' }))
    await userEvent.click(screen.getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(seen?.pathname).toBe('/addons/online/play/series/77'))
    expect(seen!.state).toMatchObject({
      ref: 'ep-2-3',
      quality: '720p',
      title: 'Дюна',
      season: 2,
      episode: 3,
      source: 'alpha',
      voice: 'Дубляж',
      subtitle: 'Сезон 2, серия 3',
    })
    expect((seen!.state as { episodes: unknown[] }).episodes).toHaveLength(3)
  })

  it('each source row shows its quality, voices and subtitles once they are known', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha'), src('beta', 'Beta')] }),
      'browse:src-alpha': () =>
        json({
          level: 'movie',
          voices: ['Дубляж', 'LostFilm', 'Оригинал', 'HDRezka', 'Кубик'].map((name, i) => ({ name, active: i === 0, ref: `a-${i}` })),
          files: [{ name: 'Фильм', voice: 'Дубляж', qualities: ['1080p'], ref: 'fa', subtitles: 2, subtitleTracks: [{ label: 'Русские', lang: 'ru' }, { label: 'English', lang: 'en' }] }],
        }),
      'browse:src-beta': () => json({ level: 'movie', files: [{ name: 'Фильм', voice: 'Оригинал', qualities: ['720p'], ref: 'fb', subtitles: 0 }] }),
    })
    await session.login(2)
    renderSheet('movie')
    const list = await screen.findByRole('radiogroup', { name: 'Источник' })
    await waitFor(() => expect(within(list).getByRole('radio', { name: /Alpha/ })).toHaveTextContent('Alpha1080pЗвук Дубляж, LostFilm, Оригинал +2Субтитры RU EN'))
    await waitFor(() => expect(within(list).getByRole('radio', { name: /Beta/ })).toHaveTextContent('Beta1080pЗвук Оригинал'))
    // the subtitles badge is the captions icon and the languages
    const alpha = within(list).getByRole('radio', { name: /Alpha/ })
    expect(alpha.querySelectorAll('svg').length).toBeGreaterThanOrEqual(2)
    // inside the source: the open voice says its subtitles, and the chosen file names them
    const voices = screen.getByRole('radiogroup', { name: 'Озвучка' })
    expect(within(voices).getByRole('radio', { name: /Дубляж/ })).toHaveTextContent('ДубляжСубтитры RU EN')
    expect(within(voices).getByRole('radio', { name: /LostFilm/ })).toHaveTextContent(/^LostFilm$/)
    expect(screen.getByTestId('summary-subtitles')).toHaveTextContent('СубтитрыРусские, English')
  })

  it('a series source row learns its voices and subtitles from its first season, in the background', async () => {
    const f = stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha'), src('beta', 'Beta')] }),
      'browse:src-alpha': () => json({ level: 'seasons', seasons: [{ number: 1, name: '', ref: 'a1' }] }),
      'browse:a1': () => json(episodesLevel(1)),
      'browse:src-beta': () => json({ level: 'seasons', seasons: [{ number: 1, name: '', ref: 'b1' }, { number: 2, name: '', ref: 'b2' }] }),
      'browse:b1': () =>
        json({
          level: 'episodes',
          season: 1,
          voices: [{ name: 'LostFilm', active: true, ref: 'b-lf' }, { name: 'Кубик в Кубе', active: false, ref: 'b-kk' }],
          episodes: eps(1).map((e) => ({ ...e, subtitles: 1, subtitleTracks: [{ label: 'English', lang: 'en' }] })),
        }),
    })
    await session.login(2)
    renderSheet()
    const list = await screen.findByRole('radiogroup', { name: 'Источник' })
    // the chosen one: its seasons say nothing, the season it opened does
    await waitFor(() => expect(within(list).getByRole('radio', { name: /Alpha/ })).toHaveTextContent('Alpha1080pЗвук Дубляж'))
    await waitFor(() => expect(within(list).getByRole('radio', { name: /Beta/ })).toHaveTextContent('Beta1080pЗвук LostFilm, Кубик в КубеСубтитры EN'))
    // only the first season is asked, never the others
    const asked = f.mock.calls.map(([u]) => new URL(String(u), 'http://x').searchParams.get('ref'))
    expect(asked).toContain('b1')
    expect(asked).not.toContain('b2')
  })

  it('episode rows show their subtitles', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      'browse:src-alpha': () =>
        json({
          ...episodesLevel(1),
          episodes: eps(1).map((e) => (e.episode === 2 ? { ...e, subtitles: 2, subtitleTracks: [{ label: 'Русские', lang: 'ru' }, { label: 'Надписи', lang: '' }] } : e)),
        }),
    })
    await session.login(2)
    renderSheet()
    const list = await screen.findByRole('radiogroup', { name: 'Серия' })
    expect(within(list).getByRole('radio', { name: /Серия 2/ })).toHaveTextContent('Серия 2Субтитры RU')
    expect(within(list).getByRole('radio', { name: /Серия 1/ })).toHaveTextContent(/^Серия 1$/)
    expect(screen.queryByTestId('summary-subtitles')).toBeNull()
    await userEvent.click(within(list).getByRole('radio', { name: /Серия 2/ }))
    expect(screen.getByTestId('summary-subtitles')).toHaveTextContent('Русские, Надписи')
  })

  it('offers: voices of the level or of its files; subtitles unknown on a seasons level', () => {
    expect(offerOf({ level: 'pick', candidates: [] })).toBeNull()
    expect(offerOf({ level: 'seasons', voices: [{ name: 'Дубляж', active: true, ref: 'x' }], seasons: [] })).toEqual({ voices: ['Дубляж'], subs: null, subLangs: [] })
    expect(offerOf(episodesLevel(1))).toEqual({ voices: ['Дубляж'], subs: false, subLangs: [] })
    // a seasons level with its first season: the season's voices and subtitles
    const season = { ...episodesLevel(1), episodes: eps(1).map((e) => ({ ...e, subtitles: 1, subtitleTracks: [{ label: 'Eng', lang: 'en' }] })) }
    expect(offerOf({ level: 'seasons', seasons: [] }, season)).toEqual({ voices: ['Дубляж'], subs: true, subLangs: ['en'] })
    expect(offerBadges({ voices: ['A', 'B'], subs: true, subLangs: [] })).toEqual(['Звук A, B', 'Субтитры'])
    expect(offerBadges({ voices: ['A', 'B', 'C', 'D'], subs: true, subLangs: ['ru', 'en', 'uk', 'de'] })).toEqual(['Звук A, B, C +1', 'Субтитры RU EN UK +1'])
    expect(offerBadges({ voices: [], subs: null, subLangs: [] })).toEqual([])
  })

  it('over the player («Другой источник») it resumes at the live position, not the progress read at mount', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      '/api/addons/online/x/progress': () =>
        json({
          entries: [{ kind: 'series', tmdbId: 77, season: 1, episode: 2, positionMs: 30_000, durationMs: 600_000, finished: false, source: 'alpha', voice: 'Дубляж', quality: '720p', updatedAt: '' }],
          last: { source: 'alpha', voice: 'Дубляж', quality: '720p', season: 1, episode: 2 },
        }),
      'browse:src-alpha': () => json(episodesLevel(1)),
    })
    await session.login(2)
    let live = 150
    render(
      <MemoryRouter initialEntries={['/addons/online/play/series/77']}>
        <Routes>
          <Route
            path="*"
            element={
              <>
                <Spy />
                <OnlineSheet kind="series" tmdbId={77} title="Дюна" onClose={vi.fn()} replace playing={{ season: 1, episode: 2, position: () => live }} />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    )
    await waitFor(() => expect(screen.getByRole('radio', { name: /Серия 2/ })).toHaveAttribute('aria-checked', 'true'))
    live = 272
    await userEvent.click(screen.getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(seen?.pathname).toBe('/addons/online/play/series/77'))
    expect(seen!.state).toMatchObject({ season: 1, episode: 2, startSec: 272 })
  })

  it('over the player, another episode leaves the resume point to the player', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      'browse:src-alpha': () => json(episodesLevel(1)),
    })
    await session.login(2)
    render(
      <MemoryRouter initialEntries={['/addons/online/play/series/77']}>
        <Routes>
          <Route
            path="*"
            element={
              <>
                <Spy />
                <OnlineSheet kind="series" tmdbId={77} title="Дюна" onClose={vi.fn()} replace playing={{ season: 1, episode: 2, position: () => 150 }} />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    )
    await userEvent.click(await screen.findByRole('radio', { name: /Серия 3/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(seen?.pathname).toBe('/addons/online/play/series/77'))
    expect(seen!.state).toMatchObject({ season: 1, episode: 3 })
    expect((seen!.state as { startSec?: number }).startSec).toBeUndefined()
  })

  it('the last choices preselect source beta and voice «Дубляж»', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha'), src('beta', 'Beta')] }),
      '/api/addons/online/x/progress': () =>
        json({ entries: [], last: { source: 'beta', voice: 'Дубляж', quality: '720p', season: 0, episode: 0 } }),
      'browse:src-beta': () =>
        json({
          level: 'movie',
          voices: [
            { name: 'Оригинал', active: true, ref: 'b-orig' },
            { name: 'Дубляж', active: false, ref: 'b-dub' },
          ],
          files: [{ name: 'Фильм', voice: 'Оригинал', qualities: ['1080p'], ref: 'f-orig', subtitles: 0 }],
        }),
      'browse:b-dub': () =>
        json({
          level: 'movie',
          voices: [
            { name: 'Оригинал', active: false, ref: 'b-orig' },
            { name: 'Дубляж', active: true, ref: 'b-dub' },
          ],
          files: [{ name: 'Фильм', voice: 'Дубляж', qualities: ['1080p', '720p'], ref: 'f-dub', subtitles: 0 }],
        }),
    })
    await session.login(2)
    renderSheet('movie')
    const voice = await screen.findByRole('radiogroup', { name: 'Озвучка' })
    await waitFor(() => expect(within(voice).getByRole('radio', { name: 'Дубляж' })).toHaveAttribute('aria-checked', 'true'))
    expect(within(screen.getByRole('radiogroup', { name: 'Источник' })).getByRole('radio', { name: /Beta/ })).toHaveAttribute('aria-checked', 'true')
    expect(within(screen.getByRole('radiogroup', { name: 'Качество' })).getByRole('radio', { name: '720p' })).toHaveAttribute('aria-checked', 'true')
  })

  it('an unsupported file is greyed «Формат не поддерживается»', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      'browse:src-alpha': () =>
        json({
          level: 'movie',
          files: [
            { name: 'Фильм', voice: 'Дубляж', qualities: ['1080p'], ref: 'f1', subtitles: 0 },
            { name: 'Фильм', voice: 'MVO', qualities: [], ref: 'f2', subtitles: 0, unsupported: true },
          ],
        }),
    })
    await session.login(2)
    renderSheet('movie')
    const mvo = await screen.findByRole('radio', { name: /MVO/ })
    expect(mvo).toBeDisabled()
    expect(mvo).toHaveTextContent('Формат не поддерживается')
    expect(screen.getByRole('radio', { name: /Дубляж/ })).toHaveAttribute('aria-checked', 'true')
  })

  it('a pick level lists the candidates', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha')] }),
      'browse:src-alpha': () =>
        json({
          level: 'pick',
          candidates: [
            { title: 'Дюна', year: 2021, ref: 'c1' },
            { title: 'Дюна', year: 1984, ref: 'c2' },
          ],
        }),
      'browse:c2': () => json({ level: 'movie', files: [{ name: 'Фильм', voice: 'Дубляж', qualities: ['720p'], ref: 'f84', subtitles: 0 }] }),
    })
    await session.login(2)
    renderSheet('movie')
    const pick = await screen.findByRole('radiogroup', { name: 'Выберите подходящий вариант' })
    expect(within(pick).getAllByRole('radio').map((r) => r.textContent)).toEqual(['Дюна (2021)', 'Дюна (1984)'])
    expect(screen.getByRole('button', { name: 'Смотреть' })).toBeDisabled()
    await userEvent.click(within(pick).getByRole('radio', { name: 'Дюна (1984)' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Смотреть' })).toBeEnabled())
  })

  it('nothing found', async () => {
    stub({ '/api/addons/online/x/sources': () => json({ sources: [] }) })
    await session.login(2)
    renderSheet()
    expect(await screen.findByText('Онлайн-источники ничего не нашли')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Повторить' })).not.toBeInTheDocument()
  })

  it('sources unavailable, with «Повторить»', async () => {
    let calls = 0
    stub({
      '/api/addons/online/x/sources': () => (++calls === 1 ? json({ error: 'Онлайн-источники сейчас недоступны' }, 503) : json({ sources: [src('alpha', 'Alpha')] })),
      'browse:src-alpha': () => json({ level: 'movie', files: [{ name: 'Фильм', voice: 'Дубляж', qualities: ['720p'], ref: 'f1', subtitles: 0 }] }),
    })
    await session.login(2)
    renderSheet('movie')
    expect(await screen.findByText('Онлайн-источники сейчас недоступны')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Повторить' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Смотреть' })).toBeEnabled())
  })

  it('TMDB down is said as the server says it', async () => {
    stub({ '/api/addons/online/x/sources': () => json({ error: 'TMDB сейчас недоступен. Попробуйте позже' }, 503) })
    await session.login(2)
    renderSheet()
    expect(await screen.findByText('TMDB сейчас недоступен. Попробуйте позже')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Повторить' })).toBeInTheDocument()
  })

  it('a source that did not answer offers the others', async () => {
    stub({
      '/api/addons/online/x/sources': () => json({ sources: [src('alpha', 'Alpha'), src('beta', 'Beta')] }),
      'browse:src-alpha': () => json({ error: 'Источник не ответил' }, 502),
      'browse:src-beta': () => json({ level: 'movie', files: [{ name: 'Фильм', voice: 'Дубляж', qualities: ['720p'], ref: 'fb', subtitles: 0 }] }),
    })
    await session.login(2)
    renderSheet('movie')
    expect(await screen.findByText('Источник не ответил')).toBeInTheDocument()
    const other = screen.getByRole('radiogroup', { name: 'Выбрать другой источник' })
    await userEvent.click(within(other).getByRole('radio', { name: /Beta/ }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Смотреть' })).toBeEnabled())
    expect(screen.queryByText('Источник не ответил')).not.toBeInTheDocument()
  })
})
