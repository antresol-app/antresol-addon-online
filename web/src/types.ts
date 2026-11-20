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
  /** Where Lampac comes from: the admin's address, the helper service on its compose name, or neither. */
  helper?: OnlineHelper
}

/** The Lampac helper service's line of the status (addonkit.HelperStatus). */
export interface OnlineHelper {
  name?: string
  source: 'override' | 'settings' | 'compose' | 'none'
  /** the compose address (never the admin's own) */
  address?: string
  /** the block to add to compose.yml, while it is not found */
  compose?: string
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

/** GET x/admin/lampac (admin): «Настройки»'s Lampac block. Never the account's value nor the address' query. */
export interface LampacSettings {
  /** The saved address ('' : none saved). */
  url: string
  /** The helper service's compose address (the field's value while none is saved). */
  composeUrl?: string
  /** Where the address in force comes from. */
  source: 'override' | 'settings' | 'compose' | 'none'
  /** The server's ANTRESOL_ONLINE_LAMPAC_URL is set: it wins over the address saved here (the sign-in saved here still applies). */
  server: boolean
  /** The account's query key ('' : no account). */
  authKey: string
  /** An account value is saved. */
  authSet: boolean
  authKeys: string[]
  /** Seconds; 0: the default. */
  timeout: number
  sourcesWait: number
  defaultTimeout: number
  defaultSourcesWait: number
  minWait: number
  maxWait: number
  /** The addon's own id for Lampac's accounts (sent with the shared password; the one for Telegram's /start). */
  uid?: string
}

/** PUT x/admin/lampac: `authValue` left out keeps the saved one; '' clears it. */
export interface LampacSave {
  url: string
  authKey: string
  authValue?: string
  timeout: number
  sourcesWait: number
}

/** One of Lampac's sources with the admin's choice (GET x/admin/lampac/sources). */
export interface LampacSourceRow {
  id: string
  name: string
  enabled: boolean
  /** Lampac found its probe title there. */
  found: boolean
  /** Lampac lists it now. */
  listed: boolean
}

/** POST x/admin/lampac/check: a real request to Lampac with the typed values. */
export interface LampacCheck {
  result: 'ok' | 'auth' | 'down' | 'error'
  sources: number
}
