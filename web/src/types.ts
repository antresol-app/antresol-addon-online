// The online server API's shapes (the addon's x/… routes): the sources of a
// title, a source's levels, the play request, the watch progress, the admin's
// status and setting.
import type { PlayRequest } from '@antresol/addon-sdk'

/** What the browser can decode: the caps of the SDK's play request. */
type ClientCaps = PlayRequest['caps']

/** GET x/sources: a working online source of a title. */
export interface OnlineSource {
  id: string
  name: string
  /** The quality hint of the source («1080p»), '' when unknown. */
  quality: string
  ref: string
}

/** A voice-over: the same level in that voice. */
export interface OnlineChoice {
  name: string
  active: boolean
  ref: string
}

export interface OnlineSeason {
  number: number
  name: string
  ref: string
}

/** A subtitle track of an online file or episode, as the picker shows it. */
export interface OnlineSubInfo {
  label: string
  lang: string
}

/** A playable episode; ref is a file ref (POST x/play). */
export interface OnlineEpisode {
  season: number
  episode: number
  name: string
  ref: string
  qualities: string[]
  maxQuality?: string
  subtitles: number
  /** Its subtitles (label as the source gives it, language code or empty); a «call» entry knows them only when played. */
  subtitleTracks?: OnlineSubInfo[]
  unsupported?: boolean
}

/** A playable film entry (usually one per voice-over). */
export interface OnlineFile {
  name: string
  voice: string
  qualities: string[]
  maxQuality?: string
  ref: string
  subtitles: number
  subtitleTracks?: OnlineSubInfo[]
  unsupported?: boolean
}

export interface OnlineCandidate {
  title: string
  year?: number
  details?: string
  ref: string
}

/** GET x/browse: one level of a source. */
export interface OnlineLevel {
  level: 'movie' | 'seasons' | 'episodes' | 'pick' | (string & {})
  voices?: OnlineChoice[]
  seasons?: OnlineSeason[]
  season?: number
  episodes?: OnlineEpisode[]
  files?: OnlineFile[]
  candidates?: OnlineCandidate[]
}

/** POST x/play. */
export interface OnlinePlayRequest {
  ref: string
  /** A source quality («1080p»); '' = the server's default. */
  quality?: string
  startSec?: number
  caps: ClientCaps
  replaceSession?: string
  refresh?: boolean
}

/** An online watch position (GET x/progress). */
export interface OnlineEntry {
  kind: 'movie' | 'series'
  tmdbId: number
  season: number
  episode: number
  positionMs: number
  durationMs: number
  finished: boolean
  source: string
  voice: string
  quality: string
  updatedAt: string
}

export interface OnlineLast {
  source: string
  voice: string
  quality: string
  season: number
  episode: number
}

export interface OnlineProgress {
  entries: OnlineEntry[]
  last: OnlineLast | null
}

/** GET x/admin/status (admin). */
export interface OnlineStatus {
  configured: boolean
  reachable: boolean
  sources: number
  version: string
}

/** PUT x/progress. */
export interface OnlineProgressSave {
  kind: 'movie' | 'series'
  tmdbId: number
  season: number
  episode: number
  positionMs: number
  durationMs: number
  source: string
  voice: string
  quality: string
  /** Series: the episode the source lists after this one (0: unknown). */
  nextSeason?: number
  nextEpisode?: number
  /** Series: the source lists no episode after this one. */
  lastListed?: boolean
  /** RFC 3339: when the position was reached (a save sent late); the server keeps the newer one. */
  updatedAt?: string
}

/** GET and PUT x/admin/settings (admin): «Смотреть онлайн» on or off. */
export interface OnlineSettings {
  enabled: boolean
}
