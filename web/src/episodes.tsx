// The episode guide's player of the online sources (episodeActions, kind
// online): an online source opens the online player (with the episodes of
// the same provider and voice, for «Следующая серия»). «Другие источники…»
// opens the online picker on that episode. Nothing is offered to a kid, nor
// while the way is off or still being asked (the guide then has no player
// for the kind: its online sources are not playable, as without the addon).
import { useUser, useWatchWay, type EpisodeCtx, type EpisodePlay, type EpisodeSource, type TitleEpisode } from '@antresol/addon-sdk'
import { useNavigate } from 'react-router'
import { episodeSubtitle, playPath, type OnlinePlayState } from './OnlineSheet'
import { useOnlineSheet, WATCH_WAY } from './ways'

/** The kinds a tap prefers once the profile chose the online way last. */
const PREFER = ['online'] as const

export function useOnlineEpisodePlay(ctx: EpisodeCtx): EpisodePlay | null {
  const { kind, tmdbId, title, poster, episodes, choice } = ctx
  const navigate = useNavigate()
  const user = useUser()
  const way = useWatchWay()
  const online = useOnlineSheet({ kind, tmdbId, title, poster })
  // a kid plays only the library (the guide does that itself); online is for
  // the profiles the way is offered to
  if (!user || user.isKid || !online.available) return null

  const playOnline = (ep: TitleEpisode, src: EpisodeSource) => {
    const provider = src.meta.providerId ?? ''
    const voice = src.meta.voice ?? ''
    const same = (s: EpisodeSource) => s.kind === 'online' && (s.meta.providerId ?? '') === provider && (s.meta.voice ?? '') === voice
    const list = episodes.flatMap((e) => {
      const s = e.sources.find(same)
      return s ? [{ season: e.season, episode: e.number, name: e.name, ref: s.ref }] : []
    })
    const series = kind === 'series'
    const state: OnlinePlayState = {
      ref: src.ref,
      // the choice's quality when this source has it; else the server's default
      quality: choice?.quality && choice.quality === src.quality ? choice.quality : '',
      title,
      subtitle: series ? episodeSubtitle(ep.season, ep.number) : undefined,
      poster,
      season: series ? ep.season : 0,
      episode: series ? ep.number : 0,
      source: provider,
      voice,
      episodes: list,
    }
    navigate(playPath(kind, tmdbId), { state })
  }

  const play = (ep: TitleEpisode, src: EpisodeSource) => {
    if (src.kind === 'online') playOnline(ep, src)
  }

  const other = (ep: TitleEpisode) => online.openAt({ season: ep.season, episode: ep.number })

  const node = <>{online.sheet}</>
  return { play, other, node, prefer: way === WATCH_WAY ? PREFER : undefined }
}
