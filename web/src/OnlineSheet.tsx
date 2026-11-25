// «Смотреть онлайн»: the picker of an online stream, in the order источник →
// озвучка → сезон → серия → качество. The server asks every source at once
// (about 10 s, «Ищем источники…»); the last choices of this title
// (x/progress) are preselected. A section with one choice is a summary row,
// not a list. «Смотреть» opens the online player
// (/addons/online/play/:kind/:tmdbId) with what it needs to play, resume and
// go on to the next episode.
import { ApiError, langBadge, languageName, ui, useUser } from '@antresol/addon-sdk'
import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { useNavigate } from 'react-router'
import { onlineBrowse, onlineProgress, onlineSources } from './api'
import { hasPending } from './pendingProgress'
import styles from './OnlineSheet.module.css'
import { tr, useT } from './i18n'
import type { OnlineChoice, OnlineEntry, OnlineEpisode, OnlineFile, OnlineLevel, OnlineProgress, OnlineSource } from './types'

const { Badges, Button, Chips, Sheet, Spinner } = ui

/** Where the full-window player is (the route `play/:kind/:tmdbId` of the addon). */
export const playPath = (kind: string, tmdbId: number) => `/addons/online/play/${kind}/${tmdbId}`

// Read at call time: the interface language may change.
const NOTHING_FOUND = () => tr('online.nothingFound')
const UNAVAILABLE = () => tr('online.unavailable')
const SOURCE_FAILED = () => tr('online.sourceFailed')
const SOURCE_EMPTY = () => tr('online.sourceEmpty')

/** What the online player gets (router state of the player's route). */
export interface OnlinePlayState {
  /** The file or episode ref (POST x/play). */
  ref: string
  /** The source quality asked for («1080p»; '' = the server's default). */
  quality: string
  title: string
  subtitle?: string
  poster?: string
  /** 0 for a movie. */
  season: number
  episode: number
  /** The source id and voice name (remembered with the progress). */
  source: string
  voice: string
  /** The episodes of this voice, in the source's order («Следующая серия»). */
  episodes: OnlineEpisodeRef[]
  /** Where to start, seconds (else the saved position). */
  startSec?: number
}

export interface OnlineEpisodeRef {
  season: number
  episode: number
  name: string
  ref: string
}

export interface OnlineSheetProps {
  kind: 'movie' | 'series'
  tmdbId: number
  title: string
  /** TMDB's seasons of a series (the source lists its own). */
  seasons?: { number: number; episodes: number }[]
  poster?: string
  onClose: () => void
  /** Replace the current history entry (the player's «Другой источник»). */
  replace?: boolean
  /**
   * What the player under the sheet plays and its live position (seconds):
   * picking another source of that file or episode resumes there, not at
   * the progress read when the sheet opened.
   */
  playing?: { season: number; episode: number; position: () => number }
  /** Where the sheet is mounted (the player's root, so it shows in full screen). */
  container?: Element | null
  /** The episode to open on (an episode's «Другие источники…»), over the remembered one. */
  at?: { season: number; episode: number }
}

/** «Сезон 2, серия 3». */
export const episodeSubtitle = (season: number, episode: number) => tr('online.episodeSubtitle', { season, episode })

/** What to look for while going down a source: the remembered (or current) voice and season. */
export interface Wanted {
  voice?: string
  season?: number
}

/** A source browsed down to something playable: a film's files, a season's episodes, or titles to pick from. */
export interface Descended {
  level: OnlineLevel
  /** The seasons level above an episodes level. */
  seasons?: OnlineLevel
}

/**
 * Browses `ref` down to a playable level: switches once to the wanted
 * voice when the source offers it, and opens the wanted season (else the
 * first). A `pick` level stops (the person picks the title).
 */
export async function descend(ref: string, want: Wanted, signal?: AbortSignal): Promise<Descended> {
  let lv = await onlineBrowse(ref, signal)
  let seasons: OnlineLevel | undefined
  let switched = false
  for (let hop = 0; hop < 6; hop++) {
    if (lv.level === 'pick') return { level: lv }
    const v = !switched && want.voice ? lv.voices?.find((x) => x.name === want.voice) : undefined
    if (v && !v.active) {
      switched = true
      lv = await onlineBrowse(v.ref, signal)
      continue
    }
    if (lv.level === 'seasons') {
      seasons = lv
      const list = lv.seasons ?? []
      const s = list.find((x) => x.number === want.season) ?? list[0]
      if (!s) throw new ApiError(404, SOURCE_EMPTY())
      lv = await onlineBrowse(s.ref, signal)
      continue
    }
    break
  }
  return { level: lv, seasons }
}

/** The voices to show: the level's own, else those of its seasons level. */
export const voicesOf = (d: Descended | null): OnlineChoice[] => (d?.level.voices?.length ? d.level.voices : (d?.seasons?.voices ?? []))

/** The voice name a film file plays in. */
export const fileVoice = (f: OnlineFile) => f.voice || f.name

/** Where to start an episode (a movie: 0, 0): its saved position, else the start. */
const resumeAt = (entries: OnlineEntry[], season: number, episode: number) => {
  const e = entryOf(entries, season, episode)
  return e && !e.finished ? e.positionMs / 1000 : 0
}

/** Saved progress of one episode (a movie: 0, 0). */
const entryOf = (entries: OnlineEntry[], season: number, episode: number) => entries.find((e) => e.season === season && e.episode === episode)

/** The height of a source quality («1080p» → 1080, «4K» → 2160; 0 when unknown). */
export function qualityHeight(q: string): number {
  if (/4k|2160/i.test(q)) return 2160
  const m = /(\d{3,4})/.exec(q)
  return m ? Number(m[1]) : 0
}

/** The quality to preselect: the remembered one, else (outside home) the best up to 1080p, else the best. */
export function defaultQuality(options: string[], remembered: string | undefined, remote: boolean): string {
  if (remembered && options.includes(remembered)) return remembered
  if (remote) {
    const fit = options.find((q) => qualityHeight(q) > 0 && qualityHeight(q) <= 1080)
    if (fit) return fit
  }
  return options[0] ?? ''
}

/**
 * What a source offers: the voice names, whether a file has subtitles
 * (null: not known yet — a series' seasons), and their languages («ru», in
 * order; a track whose label names none adds nothing).
 */
export interface SourceOffer {
  voices: string[]
  subs: boolean | null
  subLangs: string[]
}

type Playable = OnlineFile | OnlineEpisode

/** The languages of the subtitles of `items`, each once, in order. */
export const subLangsOf = (items: Playable[]): string[] => [
  ...new Set(items.flatMap((i) => (i.subtitleTracks ?? []).map((s) => s.lang.trim().toLowerCase()).filter(Boolean))),
]

/**
 * The offer of a source's first level, with the playable level under it
 * (`inner`: a series' first season) when that is known; null for a «pick»
 * level (titles, not files).
 */
export function offerOf(lv: OnlineLevel, inner?: OnlineLevel): SourceOffer | null {
  if (lv.level === 'pick') return null
  const play = inner && inner.level !== 'pick' ? inner : lv
  const files = play.files ?? []
  const named = lv.voices?.length ? lv.voices : play.voices
  const voices = named?.length ? named.map((v) => v.name) : [...new Set(files.filter((f) => !f.unsupported).map(fileVoice))]
  const items = play.level === 'movie' ? files : play.level === 'episodes' ? (play.episodes ?? []) : null
  return { voices, subs: items ? items.some((i) => i.subtitles > 0) : null, subLangs: items ? subLangsOf(items) : [] }
}

/** How many names a badge lists before «+N». */
const BADGE_VOICES = 3
const BADGE_LANGS = 3

const listed = (xs: string[], max: number, sep: string) => `${xs.slice(0, max).join(sep)}${xs.length > max ? ` +${xs.length - max}` : ''}`

/** «Субтитры RU EN» (shown as the captions icon and the languages); «Субтитры» when none is named. */
export function subsBadge(langs: string[]): string {
  const codes = [...new Set(langs.map(langBadge).filter(Boolean))]
  return codes.length ? `${tr('online.subs')} ${listed(codes, BADGE_LANGS, ' ')}` : tr('online.subs')
}

/** The subtitles badge of a file or episode ([] without subtitles). */
export const itemSubsBadges = (items: Playable[]): string[] => (items.some((i) => i.subtitles > 0) ? [subsBadge(subLangsOf(items))] : [])

/** «Звук LostFilm, HDrezka Studio, Оригинал +2», then «Субтитры RU EN». */
export function offerBadges(o: SourceOffer | null | undefined): string[] {
  if (!o) return []
  const out: string[] = []
  if (o.voices.length > 0) out.push(`${tr('online.audioBadge')} ${listed(o.voices, BADGE_VOICES, ', ')}`)
  if (o.subs) out.push(subsBadge(o.subLangs))
  return out
}

/** «Русские, English» for the chosen file or episode ('' without subtitles). */
export function subtitleNames(item: Playable | undefined): string {
  if (!item || item.subtitles <= 0) return ''
  const tracks = item.subtitleTracks ?? []
  if (tracks.length === 0) return ''
  const names = tracks.map((s, i) => s.label.trim() || languageName(s.lang) || tr('player.track.subtitlesN', { n: i + 1 }))
  return [...new Set(names)].join(', ')
}

/**
 * How many sources are asked for their offer at once: one, and only after
 * the chosen source opened — they share the profile's two Lampac slots
 * with what the person is waiting for.
 */
const OFFER_PARALLEL = 1

/** An offer that already says whether there are subtitles (a better one need not replace it). */
const knowsSubs = (o: SourceOffer | null | undefined) => Boolean(o && o.subs !== null)

type Load = { status: 'loading' } | { status: 'error'; message: string; retry: boolean } | { status: 'ready'; sources: OnlineSource[] }
type Browse = { status: 'idle' } | { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; d: Descended }

function sourcesError(e: unknown): { message: string; retry: boolean } {
  if (e instanceof ApiError) {
    if (e.status === 503) return { message: e.message.startsWith('TMDB') ? e.message : UNAVAILABLE(), retry: true }
    if (e.status === 404) return { message: NOTHING_FOUND(), retry: false }
    return { message: e.message, retry: e.status === 0 || e.status >= 500 }
  }
  return { message: UNAVAILABLE(), retry: true }
}

function browseError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 502) return SOURCE_FAILED()
    if (e.status === 404) return SOURCE_EMPTY()
    if (e.status === 503 && !e.message.startsWith('TMDB')) return UNAVAILABLE()
    return e.message
  }
  return SOURCE_FAILED()
}

export function OnlineSheet({ kind, tmdbId, title, poster, onClose, replace, playing, container, at }: OnlineSheetProps) {
  const t = useT()
  const navigate = useNavigate()
  const user = useUser()
  const [attempt, setAttempt] = useState(0)
  const [load, setLoad] = useState<Load>({ status: 'loading' })
  const [progress, setProgress] = useState<OnlineProgress | null>(null)
  const [source, setSource] = useState('')
  const [browse, setBrowse] = useState<Browse>({ status: 'idle' })
  const [fileRef, setFileRef] = useState('')
  const [episodeNo, setEpisodeNo] = useState(0)
  const [quality, setQuality] = useState('')
  const [offers, setOffers] = useState<Record<string, SourceOffer | null>>({})
  const browseAc = useRef<AbortController | null>(null)
  const seasonsRef = useRef<OnlineLevel | undefined>(undefined)
  const last = progress?.last ?? null
  const entries = useMemo(() => progress?.entries ?? [], [progress])

  // The sources and the last choices, together (a failed progress read only loses the preselection).
  useEffect(() => {
    const ac = new AbortController()
    setLoad({ status: 'loading' })
    setBrowse({ status: 'idle' })
    const prog = onlineProgress(kind, tmdbId, ac.signal).catch(() => null)
    Promise.all([onlineSources(kind, tmdbId, ac.signal), prog]).then(
      ([sources, p]) => {
        setProgress(p)
        if (sources.length === 0) return setLoad({ status: 'error', message: NOTHING_FOUND(), retry: false })
        setLoad({ status: 'ready', sources })
        const remembered = p?.last?.source ? sources.find((s) => s.id === p.last!.source) : undefined
        setSource((remembered ?? sources[0]!).id)
      },
      (e: unknown) => {
        if (ac.signal.aborted) return
        setLoad({ status: 'error', ...sourcesError(e) })
      },
    )
    return () => ac.abort()
  }, [kind, tmdbId, attempt])
  useEffect(() => () => browseAc.current?.abort(), [])

  // Every other source's first level, one at a time once the chosen source
  // has opened, for the badges of its row (Lampac caches what was asked; a
  // failed source just has none); a series' seasons say nothing of voices or
  // subtitles, so its first season is asked too. The chosen one's offer is
  // what it opened.
  const loaded = load.status === 'ready' ? load.sources : null
  const firstOpened = browse.status === 'ready' || browse.status === 'error'
  const [offersStarted, setOffersStarted] = useState(false)
  useEffect(() => {
    if (firstOpened) setOffersStarted(true)
  }, [firstOpened])
  useEffect(() => {
    if (browse.status !== 'ready' || !source) return
    const { seasons, level } = browse.d
    // the first offer stays (another voice's files do not change the row), unless it knew less
    setOffers((o) => (knowsSubs(o[source]) ? o : { ...o, [source]: offerOf(seasons ?? level, seasons ? level : undefined) }))
  }, [browse, source])
  useEffect(() => {
    if (!loaded || !offersStarted) return
    const ac = new AbortController()
    const queue = loaded.filter((s) => s.id !== source)
    const next = async (): Promise<void> => {
      const s = queue.shift()
      if (!s || ac.signal.aborted) return
      try {
        const lv = await onlineBrowse(s.ref, ac.signal)
        if (ac.signal.aborted) return
        setOffers((o) => (o[s.id] ? o : { ...o, [s.id]: offerOf(lv) }))
        const first = lv.level === 'seasons' ? lv.seasons?.[0] : undefined
        if (first) {
          const inner = await onlineBrowse(first.ref, ac.signal).catch(() => null)
          if (inner && !ac.signal.aborted) setOffers((o) => (knowsSubs(o[s.id]) ? o : { ...o, [s.id]: offerOf(lv, inner) }))
        }
      } catch {
        // no badges for this one
      }
      return next()
    }
    for (let i = 0; i < OFFER_PARALLEL; i++) void next()
    return () => ac.abort()
    // started once per search: switching sources does not ask them all again
  }, [loaded, offersStarted])

  /** Goes down `ref`, keeping the voice, season and episode asked for where the source has them. */
  const open = useCallback(
    (ref: string, want: Wanted & { episode?: number; file?: string; keepSeasons?: boolean; exact?: boolean }) => {
      browseAc.current?.abort()
      const ac = new AbortController()
      browseAc.current = ac
      setBrowse({ status: 'loading' })
      const above = want.keepSeasons ? seasonsRef.current : undefined
      descend(ref, want, ac.signal).then(
        (got) => {
          if (ac.signal.aborted) return
          // a season or voice of an episodes level answers with episodes only: the seasons stay
          const d = !got.seasons && got.level.level === 'episodes' && above ? { ...got, seasons: above } : got
          seasonsRef.current = d.seasons
          setBrowse({ status: 'ready', d })
          const lv = d.level
          if (lv.level === 'episodes') {
            const eps = lv.episodes ?? []
            const season = lv.season ?? eps[0]?.season ?? 0
            let ep = eps.find((e) => e.episode === want.episode && !e.unsupported)
            // the remembered episode was watched to the end: offer the next one
            if (ep && !want.exact && entryOf(entries, season, ep.episode)?.finished) ep = eps[eps.indexOf(ep) + 1] ?? ep
            setEpisodeNo((ep ?? eps.find((e) => !e.unsupported) ?? eps[0])?.episode ?? 0)
          } else if (lv.level === 'movie') {
            const files = lv.files ?? []
            const f =
              files.find((x) => x.ref === want.file) ??
              (want.voice ? files.find((x) => !x.unsupported && fileVoice(x) === want.voice) : undefined) ??
              files.find((x) => !x.unsupported) ??
              files[0]
            setFileRef(f?.ref ?? '')
          }
        },
        (e: unknown) => !ac.signal.aborted && setBrowse({ status: 'error', message: browseError(e) }),
      )
    },
    [entries],
  )

  // A source chosen (or preselected): browse it with the remembered choices.
  const sources = load.status === 'ready' ? load.sources : []
  const src = sources.find((s) => s.id === source)
  useEffect(() => {
    if (!src) return
    const same = last && last.source === src.id
    open(src.ref, {
      voice: last?.voice || undefined,
      season: at ? at.season : same || kind === 'series' ? last?.season : undefined,
      episode: at ? at.episode : last?.episode,
      exact: Boolean(at),
    })
    // a new source only; the remembered choices are read once with it
  }, [src?.id])

  const d = browse.status === 'ready' ? browse.d : null
  const lv = d?.level
  const voices = voicesOf(d)
  const activeVoice = voices.find((v) => v.active)?.name ?? ''
  const files = lv?.level === 'movie' ? (lv.files ?? []) : []
  const episodes = lv?.level === 'episodes' ? (lv.episodes ?? []) : []
  const season = lv?.level === 'episodes' ? (lv.season ?? episodes[0]?.season ?? 0) : 0
  const file = files.find((f) => f.ref === fileRef)
  const episode = episodes.find((e) => e.episode === episodeNo)
  const item: OnlineFile | OnlineEpisode | undefined = lv?.level === 'movie' ? file : episode
  const qualities = item && !item.unsupported ? item.qualities : []
  const itemKey = item?.ref ?? ''

  // Each file or episode has its own qualities: keep the chosen one when it has it.
  useEffect(() => {
    if (!item) return
    setQuality((q) => (q && item.qualities.includes(q) ? q : defaultQuality(item.qualities, last?.quality, Boolean(user?.remote))))
  }, [itemKey])

  const voice = activeVoice || (file ? fileVoice(file) : '')
  // The subtitles of each voice seen so far (its files or this season's
  // episodes), for the badges of the voice rows: another voice's are known
  // only once it was opened.
  const [voiceSubs, setVoiceSubs] = useState<Record<string, string[]>>({})
  const voiceKey = (name: string) => `${source}\u0000${name}`
  const shownItems: Playable[] = lv?.level === 'movie' ? files : episodes
  useEffect(() => {
    if (!activeVoice || shownItems.length === 0) return
    const k = voiceKey(activeVoice)
    const b = itemSubsBadges(shownItems)
    setVoiceSubs((m) => (m[k] && m[k].join('\n') === b.join('\n') ? m : { ...m, [k]: b }))
  }, [lv, activeVoice])
  const canWatch = Boolean(item && !item.unsupported && src)
  const subNames = item && !item.unsupported ? subtitleNames(item) : ''
  // Episode rows carry their subtitles only when the episodes differ: the
  // same badge on every row says less than the voice row and «Субтитры» below.
  const episodeSubsDiffer = new Set(episodes.filter((e) => !e.unsupported).map((e) => itemSubsBadges([e]).join('\n'))).size > 1
  // Over the player, the progress read at mount is behind it: the live
  // position of what it plays, else none (the player asks the server
  // after this device's pending saves). Else the resume point is already
  // here, and the player need not ask again (unless this device keeps
  // newer positions the server lacks).
  const startAt = (s: number, e: number): number | undefined => {
    if (replace) {
      if (!playing || playing.season !== s || playing.episode !== e) return undefined
      const at = playing.position()
      return Number.isFinite(at) && at > 0 ? at : undefined
    }
    return progress && !hasPending(user?.id ?? 0) ? resumeAt(entries, s, e) : undefined
  }
  const watch = () => {
    if (!item || item.unsupported || !src) return
    const state: OnlinePlayState = {
      ref: item.ref,
      quality: qualities.includes(quality) ? quality : '',
      title,
      subtitle: lv?.level === 'episodes' ? episodeSubtitle(season, episodeNo) : undefined,
      poster,
      season: lv?.level === 'episodes' ? season : 0,
      episode: lv?.level === 'episodes' ? episodeNo : 0,
      source: src.id,
      voice,
      episodes: episodes.filter((e) => !e.unsupported).map((e) => ({ season: e.season, episode: e.episode, name: e.name, ref: e.ref })),
      startSec: startAt(lv?.level === 'episodes' ? season : 0, lv?.level === 'episodes' ? episodeNo : 0),
    }
    onClose()
    navigate(playPath(kind, tmdbId), { state, replace })
  }

  const chooseVoice = (name: string) => {
    if (lv?.level === 'movie' && voices.length === 0) return setFileRef(files.find((f) => fileVoice(f) === name)?.ref ?? '')
    const v = voices.find((x) => x.name === name)
    if (v && !v.active) open(v.ref, { voice: name, season, episode: episodeNo, keepSeasons: true })
  }
  const chooseSeason = (n: number) => {
    const s = d?.seasons?.seasons?.find((x) => x.number === n)
    if (s && n !== season) open(s.ref, { voice: activeVoice || undefined, season: n, keepSeasons: true })
  }

  const sourceList = (label: string, expand: boolean) => (
    <Choices
      label={label}
      options={sources.map((s) => ({ key: s.id, label: s.name, badges: [s.quality, ...offerBadges(offers[s.id])].filter(Boolean) }))}
      value={source}
      onChange={(id) => (id === source && src ? open(src.ref, { voice: activeVoice || undefined, season }) : setSource(id))}
      expand={expand}
    />
  )

  let body: ReactNode
  if (load.status === 'loading') {
    body = (
      <div className={styles.searching} role="status">
        <Spinner track className={styles.spinner} />
        <span>{t('online.searching')}</span>
        <span className={styles.bar} aria-hidden>
          <span />
        </span>
      </div>
    )
  } else if (load.status === 'error') {
    body = (
      <div className={styles.problem} role="alert">
        <p>{load.message}</p>
        {load.retry && (
          <Button variant="secondary" size="small" icon="restart" onClick={() => setAttempt((n) => n + 1)}>
            {t('common.retry')}
          </Button>
        )}
      </div>
    )
  } else {
    body = (
      <div className={styles.steps}>
        {browse.status === 'error' ? (
          <>
            <p className={styles.problem} role="alert">
              {browse.message}
            </p>
            {sources.length > 1 ? (
              sourceList(t('online.pickOtherSource'), true)
            ) : (
              <div>
                <Button variant="secondary" size="small" icon="restart" onClick={() => src && open(src.ref, {})}>
                  {t('common.retry')}
                </Button>
              </div>
            )}
          </>
        ) : (
          sourceList(t('online.source'), false)
        )}
        {browse.status === 'loading' && (
          <div className={styles.searching} role="status">
            <Spinner track className={styles.spinner} />
            <span className="visually-hidden">{t('common.loading')}</span>
          </div>
        )}
        {lv?.level === 'pick' && (
          <Choices
            label={t('online.pickMatch')}
            options={(lv.candidates ?? []).map((c) => ({ key: c.ref, label: c.year ? `${c.title} (${c.year})` : c.title, detail: c.details }))}
            value=""
            onChange={(ref) => open(ref, { voice: last?.voice || undefined, season: last?.season, episode: last?.episode })}
            expand
          />
        )}
        {lv && lv.level !== 'pick' && (voices.length > 0 || files.length > 0) && (
          <Choices
            label={t('online.voice')}
            options={
              voices.length > 0
                ? voices.map((v) => ({ key: v.name, label: v.name, badges: voiceSubs[voiceKey(v.name)] }))
                : files.map((f) => ({
                    key: fileVoice(f),
                    label: fileVoice(f),
                    disabled: f.unsupported,
                    detail: f.unsupported ? t('online.unsupported') : undefined,
                    badges: f.unsupported ? undefined : itemSubsBadges([f]),
                  }))
            }
            value={voice}
            onChange={chooseVoice}
          />
        )}
        {lv?.level === 'episodes' && (
          <>
            <Choices
              label={t('common.kind.season')}
              layout="chips"
              options={(d?.seasons?.seasons ?? [{ number: season, name: '', ref: '' }]).map((s) => ({ key: String(s.number), label: t('online.season', { n: s.number }) }))}
              value={String(season)}
              onChange={(n) => chooseSeason(Number(n))}
            />
            <Choices
              label={t('common.kind.episode')}
              options={episodes.map((e) => {
                const p = entryOf(entries, e.season, e.episode)
                // «Серия 3», «3 серия», «Episode 3» only repeat the row's own label
                const name = e.name && !/^(?:(?:серия|episode)\s*\d+|\d+\s*(?:-?я\s*)?(?:серия|episode))$/i.test(e.name.trim()) ? e.name : ''
                return {
                  key: String(e.episode),
                  label: t('online.episode', { n: e.episode }),
                  detail: e.unsupported ? t('online.unsupported') : name,
                  disabled: e.unsupported,
                  badges: e.unsupported || !episodeSubsDiffer ? undefined : itemSubsBadges([e]),
                  progress: p && !p.finished && p.durationMs > 0 ? p.positionMs / p.durationMs : undefined,
                }
              })}
              value={String(episodeNo)}
              onChange={(n) => setEpisodeNo(Number(n))}
            />
          </>
        )}
        {qualities.length > 0 && <Choices label={t('common.quality')} layout="chips" options={qualities.map((q) => ({ key: q, label: q }))} value={quality} onChange={setQuality} />}
        {subNames && (
          <div className={styles.summary} data-testid="summary-subtitles">
            <span className={styles.summaryLabel}>{t('online.subs')}</span>
            <span className={styles.summaryValue}>{subNames}</span>
          </div>
        )}
        {lv && lv.level !== 'pick' && item?.unsupported && (
          <p className={styles.note} role="status">
            {t('online.unsupported')}
          </p>
        )}
      </div>
    )
  }

  // One live region for the whole search: it says when the sources arrived
  // (the spinner's own status node goes away with them).
  const announce = load.status === 'ready' && src ? t('online.sourceAnnounce', { name: src.name }) : ''
  return (
    <Sheet
      container={container}
      title={t('common.watchOnline')}
      message={title}
      role="dialog"
      size="large"
      dismissOnOutside
      onCancel={onClose}
      actions={[
        { label: t('common.watch'), kind: 'primary', disabled: !canWatch || browse.status !== 'ready', run: watch },
      ]}
    >
      {body}
      <p className="visually-hidden" role="status" aria-live="polite">
        {announce}
      </p>
    </Sheet>
  )
}

interface Option {
  key: string
  label: string
  detail?: string
  disabled?: boolean
  /** 0..1: a resume bar (an episode started before). */
  progress?: number
  /** Rows: quiet pills under the label (a source's quality, voices, subtitles). */
  badges?: string[]
}

/**
 * One step of the picker: a radio group of rows or chips. A step with one
 * choice is a summary row («Озвучка · Дубляж») unless `expand`. One tab stop
 * per group; ↑/↓/←/→ move to the next choice and choose it (a radio group).
 */
function Choices({
  label,
  options,
  value,
  onChange,
  layout = 'rows',
  expand = false,
}: {
  label: string
  options: Option[]
  value: string
  onChange: (key: string) => void
  layout?: 'rows' | 'chips'
  expand?: boolean
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([])
  if (options.length === 0) return null
  if (options.length === 1 && !expand) {
    const o = options[0]!
    return (
      <div className={styles.summary} data-testid={`summary-${label}`}>
        <span className={styles.summaryLabel}>{label}</span>
        <span className={styles.summaryValue}>{o.detail && o.disabled ? `${o.label} · ${o.detail}` : o.label}</span>
      </div>
    )
  }
  const enabled = options.map((o, i) => (o.disabled ? -1 : i)).filter((i) => i >= 0)
  const chosen = options.findIndex((o) => o.key === value && !o.disabled)
  const tabStop = chosen >= 0 ? chosen : (enabled[0] ?? -1)
  const onKey = (e: KeyboardEvent, i: number) => {
    const d = e.key === 'ArrowDown' || e.key === 'ArrowRight' ? 1 : e.key === 'ArrowUp' || e.key === 'ArrowLeft' ? -1 : 0
    if (!d || enabled.length === 0) return
    e.preventDefault()
    const at = enabled.indexOf(i)
    const n = enabled[(at + d + enabled.length) % enabled.length]!
    refs.current[n]?.focus()
    onChange(options[n]!.key)
  }
  const radio = (o: Option, i: number) => ({
    ref: (el: HTMLButtonElement | null) => {
      refs.current[i] = el
    },
    role: 'radio' as const,
    'aria-checked': o.key === value,
    tabIndex: i === tabStop ? 0 : -1,
    disabled: o.disabled,
    onClick: () => onChange(o.key),
    onKeyDown: (e: KeyboardEvent) => onKey(e, i),
  })
  if (layout === 'chips') {
    return (
      <section className={styles.step}>
        <h3 className={styles.stepTitle}>{label}</h3>
        <Chips label={label} chips={options.map((o) => ({ value: o.key, label: o.label }))} value={value} onChange={onChange} />
      </section>
    )
  }
  return (
    <section className={styles.step}>
      <h3 className={styles.stepTitle}>{label}</h3>
      <div role="radiogroup" aria-label={label} className={styles.rows}>
        {options.map((o, i) => {
          const on = o.key === value
          return (
            <button key={o.key} type="button" className={`${styles.row} ${on ? styles.rowOn : ''}`} {...radio(o, i)}>
              <span className={styles.rowText}>
                <span className={styles.rowLabel}>{o.label}</span>
                {o.detail && <span className={styles.rowDetail}>{o.detail}</span>}
                <Badges items={o.badges} inline className={styles.rowBadges} />
              </span>
              {o.progress !== undefined && (
                <span className={styles.resume} aria-hidden>
                  <span style={{ width: `${Math.round(Math.min(Math.max(o.progress, 0), 1) * 100)}%` }} />
                </span>
              )}
              <span className={styles.check} aria-hidden>
                {on && (
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" strokeLinejoin="round">
                    <path d="m5 12.5 4.5 4.5L19 7.5" />
                  </svg>
                )}
              </span>
            </button>
          )
        })}
      </div>
    </section>
  )
}
