// /addons/online/play/:kind/:tmdbId — «Смотреть онлайн» in the full-window
// player (the SDK's ui.Player). The picker (OnlineSheet) hands over what to
// play in the router state. Without it (a «Продолжить просмотр» card:
// ?s=&e=&t=) the remembered source, voice and quality are looked up again;
// when that fails — the source or the episode is gone — the title page opens
// with the picker.
//
// Progress goes to x/progress every 10 s, on pause and on close, with the
// episode the source lists next. A save the server cannot take now (503,
// offline) is kept on this device and sent again later.
import { core, ui, useUser, yieldSession, type MediaFile, type PlayRequest, type StreamPlayback, type UpNext } from '@antresol/addon-sdk'
import { Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useLocation, useNavigate, useParams, useSearchParams } from 'react-router'
import { onlinePlay, onlineProgress, onlineSource, onlineSources, onlineStop } from './api'
import { descend, episodeSubtitle, fileVoice, OnlineSheet, playPath, type Descended, type OnlinePlayState } from './OnlineSheet'
import sheet from './OnlineSheet.module.css'
import styles from './OnlinePlayer.module.css'
import { flushPending, saveOnline, usePendingRetry } from './pendingProgress'
import { useT } from './i18n'
import { setTitle as setWindowTitle } from './title'
import type { OnlineSource } from './types'
import { WATCH_WAY } from './ways'

const { Button, Player } = ui

type Kind = 'movie' | 'series'

/** The title page with the picker open (its way, run once on load): where a failed resume goes. */
export const pickerHref = (kind: Kind, tmdbId: number) => `/title/${kind}/${tmdbId}?way=${WATCH_WAY}`

// ---------- resuming without the picker ----------

/**
 * The remembered choices of a title played again: its source, voice,
 * season/episode and quality, browsed fresh (refs are never stored). The
 * source's ref is asked for alone (no wait for the list of sources, 3–10
 * s); when that source does not answer, the list decides. Throws when
 * anything is gone.
 */
export async function resumeChoice(kind: Kind, tmdbId: number, season: number, episode: number, signal?: AbortSignal): Promise<OnlinePlayState> {
  const prog = await onlineProgress(kind, tmdbId, signal)
  const last = prog.last
  if (!last?.source) throw new Error('nothing remembered')
  const want = kind === 'series' ? { s: season || last.season, e: episode || last.episode } : { s: 0, e: 0 }
  const wanted = { voice: last.voice || undefined, season: want.s }
  let src: OnlineSource | undefined
  let d: Descended
  try {
    src = await onlineSource(kind, tmdbId, last.source, signal)
    d = await descend(src.ref, wanted, signal)
  } catch (e) {
    if (signal?.aborted) throw e
    src = (await onlineSources(kind, tmdbId, signal)).find((s) => s.id === last.source)
    if (!src) throw new Error('source gone')
    d = await descend(src.ref, wanted, signal)
  }
  const lv = d.level
  if (kind === 'movie') {
    if (lv.level !== 'movie') throw new Error('not a film')
    const files = (lv.files ?? []).filter((f) => !f.unsupported)
    const active = (lv.voices ?? []).find((v) => v.active)?.name
    const f = last.voice && !active ? files.find((x) => fileVoice(x) === last.voice) : files[0]
    if (!f || (active && last.voice && active !== last.voice)) throw new Error('voice gone')
    return {
      ref: f.ref,
      quality: f.qualities.includes(last.quality) ? last.quality : '',
      title: '',
      season: 0,
      episode: 0,
      source: src.id,
      voice: active || fileVoice(f),
      episodes: [],
    }
  }
  if (lv.level !== 'episodes') throw new Error('not a season')
  const active = voicesActive(d)
  if (last.voice && active && active !== last.voice) throw new Error('voice gone')
  const eps = (lv.episodes ?? []).filter((x) => !x.unsupported)
  // an optimistic «next episode» may not exist in this source
  const ep = eps.find((x) => x.season === want.s && x.episode === want.e)
  if (!ep) throw new Error('episode gone')
  return {
    ref: ep.ref,
    quality: ep.qualities.includes(last.quality) ? last.quality : '',
    title: '',
    subtitle: episodeSubtitle(ep.season, ep.episode),
    season: ep.season,
    episode: ep.episode,
    source: src.id,
    voice: active || last.voice,
    episodes: eps.map((x) => ({ season: x.season, episode: x.episode, name: x.name, ref: x.ref })),
  }
}

const voicesActive = (d: Descended) =>
  ((d.level.voices?.length ? d.level.voices : d.seasons?.voices) ?? []).find((v) => v.active)?.name ?? ''

// ---------- the route ----------

export function OnlinePlayer() {
  const t = useT()
  // Video and music never play over each other (ui.Player does not do it).
  useLayoutEffect(() => yieldSession(), [])
  const userId = useUser()?.id ?? 0
  const { kind: rawKind = '', tmdbId: rawId = '' } = useParams()
  const kind: Kind = rawKind === 'series' ? 'series' : 'movie'
  const tmdbId = Number(rawId) || 0
  const location = useLocation()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const given = (location.state ?? null) as OnlinePlayState | null
  const [resumed, setResumed] = useState<OnlinePlayState | null>(null)
  const state = given?.ref ? given : resumed

  // A «Продолжить просмотр» card: find the remembered choices again.
  useEffect(() => {
    if (given?.ref || !tmdbId) return
    const ac = new AbortController()
    const s = Number(params.get('s')) || 0
    const e = Number(params.get('e')) || 0
    const t = Number(params.get('t')) || 0
    // positions kept on this device go first, so the resume point is the newest
    flushPending(userId, () => !ac.signal.aborted)
      .then(() => resumeChoice(kind, tmdbId, s, e, ac.signal))
      .then(
      (st) => setResumed({ ...st, title: given?.title ?? '', poster: given?.poster, startSec: t > 0 ? t : undefined }),
      () => !ac.signal.aborted && navigate(pickerHref(kind, tmdbId), { replace: true }),
    )
    return () => ac.abort()
    // the card's title and poster ride along; only a new title or card restarts this
  }, [given?.ref, kind, tmdbId, params, navigate, userId])

  if (!state) {
    // a card: the remembered source is looked up again (a second or so; the whole list, about 10 s, when it is gone)
    return (
      <div className={`${styles.playerFallback} ${sheet.fallback}`}>
        <div className={sheet.searching} role="status">
          <span className={sheet.spinner} aria-hidden />
          <span>{t('online.searching')}</span>
        </div>
      </div>
    )
  }
  // every navigation is a new session (another file, or the same file in another quality)
  return <OnlineSession key={location.key} kind={kind} tmdbId={tmdbId} state={state} />
}

/** One file or episode: its resume point, then the player. */
function OnlineSession({ kind, tmdbId, state }: { kind: Kind; tmdbId: number; state: OnlinePlayState }) {
  const t = useT()
  const navigate = useNavigate()
  const location = useLocation()
  const userId = useUser()?.id ?? 0
  // positions the server could not take are sent again while this page is open
  usePendingRetry(userId)
  const [startSec, setStartSec] = useState<number | null>(state.startSec ?? null)
  const [picking, setPicking] = useState(false)
  const [title, setTitle] = useState(state.title)
  const quality = useRef(state.quality)

  // The resume point of this file or episode (unless the card gave it).
  useEffect(() => {
    if (state.startSec !== undefined) return
    const ac = new AbortController()
    // positions kept on this device go first, so the resume point is the newest
    flushPending(userId, () => !ac.signal.aborted)
      .then(() => onlineProgress(kind, tmdbId, ac.signal))
      .then(
      (p) => {
        const e = p.entries.find((x) => x.season === state.season && x.episode === state.episode)
        setStartSec(e && !e.finished ? e.positionMs / 1000 : 0)
      },
      () => !ac.signal.aborted && setStartSec(0),
    )
    return () => ac.abort()
  }, [kind, tmdbId, state, userId])

  useEffect(() => {
    setWindowTitle(title || t('common.watchOnline'))
  }, [title])
  // opened by a link without the title (the card carries it in the router state)
  useEffect(() => {
    if (state.title) return
    const ac = new AbortController()
    core.getTitle(kind, tmdbId, ac.signal).then(
      (t) => setTitle(t.title),
      () => {},
    )
    return () => ac.abort()
  }, [kind, tmdbId, state.title])

  const idx = state.episodes.findIndex((e) => e.season === state.season && e.episode === state.episode)
  const nextEp = idx >= 0 ? state.episodes[idx + 1] : undefined

  const saver = useCallback(
    (positionMs: number, durationMs: number, keepalive: boolean) =>
      saveOnline(
        userId,
        {
          kind,
          tmdbId,
          season: state.season,
          episode: state.episode,
          positionMs,
          durationMs,
          source: state.source,
          voice: state.voice,
          quality: quality.current,
          ...(kind === 'series' && nextEp ? { nextSeason: nextEp.season, nextEpisode: nextEp.episode } : {}),
          ...(kind === 'series' && idx >= 0 && !nextEp ? { lastListed: true } : {}),
        },
        keepalive,
      ),
    [kind, tmdbId, state, nextEp, idx, userId],
  )

  const close = useCallback(() => {
    if (location.key !== 'default' && window.history.length > 1) navigate(-1)
    else navigate(`/title/${kind}/${tmdbId}`, { replace: true })
  }, [location.key, navigate, kind, tmdbId])

  // the picker opens inside the player (full screen stays as it is)
  const otherSource = useCallback(() => setPicking(true), [])

  const stream = useMemo<StreamPlayback>(
    () => ({
      stop: (session, keepalive) => onlineStop(session, keepalive),
      initialQuality: state.quality,
      saveProgress: saver,
      onOtherSource: otherSource,
      onQuality: (q) => (quality.current = q),
    }),
    [state.quality, saver, otherSource],
  )

  const start = useCallback(
    (_f: MediaFile, body: PlayRequest) =>
      onlinePlay({
        ref: state.ref,
        quality: typeof body.quality === 'string' ? body.quality : quality.current,
        startSec: body.startSec,
        caps: body.caps,
        replaceSession: body.replaceSession,
        refresh: body.refresh,
      }),
    [state.ref],
  )
  // One part, named after the title (usePlayback loads once per mount).
  const parts = useMemo<MediaFile[]>(() => [{ id: 0, name: title, size: 0, audio: [], subs: [] }], [title])

  const next: UpNext | undefined = nextEp
    ? {
        code: t('online.episodeCode', { season: nextEp.season, episode: nextEp.episode }),
        title: nextEp.name && !/^(серия|episode)\s*\d+$/i.test(nextEp.name.trim()) ? nextEp.name : t('online.episode', { n: nextEp.episode }),
        image: state.poster,
        play: () =>
          navigate(playPath(kind, tmdbId), {
            replace: true,
            state: {
              ...state,
              ref: nextEp.ref,
              quality: quality.current,
              title,
              season: nextEp.season,
              episode: nextEp.episode,
              subtitle: episodeSubtitle(nextEp.season, nextEp.episode),
              startSec: undefined,
            } satisfies OnlinePlayState,
          }),
      }
    : undefined

  if (startSec === null) {
    return (
      <div className={`${styles.playerFallback} ${sheet.fallback}`} role="status">
        <span className={sheet.spinner} aria-hidden />
        <span className="visually-hidden">{t('common.loading')}</span>
      </div>
    )
  }
  return (
    <Suspense fallback={<div className={styles.playerFallback} />}>
      <Player
        itemId={0}
        title={title || t('common.watchOnline')}
        subtitle={state.subtitle}
        artwork={state.poster}
        parts={parts}
        startSec={startSec}
        next={next}
        onClose={close}
        start={start}
        stream={stream}
        suspendKeys={picking}
        overlayOpen={picking}
        topActions={
          <Button size="small" variant="secondary" icon="sliders" onClick={otherSource}>
            {t('online.otherSource')}
          </Button>
        }
        overlay={(root, position) =>
          picking && (
            <OnlineSheet
              kind={kind}
              tmdbId={tmdbId}
              title={title}
              poster={state.poster}
              replace
              playing={{ season: state.season, episode: state.episode, position }}
              container={root}
              onClose={() => setPicking(false)}
            />
          )
        }
      />
    </Suspense>
  )
}
