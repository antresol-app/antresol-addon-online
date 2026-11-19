// The addon's own server API, through the core's proxy
// (/api/addons/online/x/…): the sources of a title, a source's levels, play,
// the stream's session, the watch progress, whether the way is offered, and
// the admin's status and setting. The shapes are types.ts's.
import { addonApi, ApiError, locale } from '@antresol/addon-sdk'
import type { PlayResponse } from '@antresol/addon-sdk'
import { tr } from './i18n'
import type { LampacCheck, LampacSave, LampacSettings, LampacSourceRow, OnlineLevel, OnlinePlayRequest, OnlineProgress, OnlineProgressSave, OnlineSettings, OnlineSource, OnlineStatus } from './types'

export const ID = 'online'
const api = addonApi(ID)

/** `?a=1&b=x` of the given values (none: ''); an empty one is left out. */
const qs = (params: Record<string, string | number>): string => {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== '') p.set(k, String(v))
  const s = p.toString()
  return s ? `?${s}` : ''
}

/**
 * A call that has to outlive the page (the last position, the stream's
 * stop, sent as the page is leaving). The SDK's `addonApi` has no
 * `keepalive`, so it goes to the same path under the proxy itself, with
 * what the app's own calls send: JSON, the session cookie, the language.
 */
async function leaving(method: 'PUT' | 'DELETE', path: string, body: unknown): Promise<void> {
  let res: Response
  try {
    res = await fetch(`/api/addons/${ID}/x/${path}`, {
      method,
      credentials: 'same-origin',
      keepalive: true,
      headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'Accept-Language': locale().slice(0, 2) },
      body: JSON.stringify(body),
    })
  } catch {
    throw new ApiError(0, tr('common.genericError'))
  }
  if (res.ok) return
  const said = (await res.json().catch(() => null)) as { error?: unknown } | null
  throw new ApiError(res.status, typeof said?.error === 'string' ? said.error : tr('common.genericError'))
}

// --- «Смотреть онлайн» (adults only; 404 while it is off) ---
/** The working online sources of a title (the server asks them all: about 10 s). */
export const onlineSources = (kind: 'movie' | 'series', tmdbId: number, signal?: AbortSignal) =>
  api.get<{ sources: OnlineSource[] }>(`sources${qs({ kind, tmdbId })}`, signal).then((r) => r.sources ?? [])
/** One remembered source of a title, without waiting for the whole list (a resume). */
export const onlineSource = (kind: 'movie' | 'series', tmdbId: number, source: string, signal?: AbortSignal) =>
  api.get<{ source: OnlineSource }>(`source${qs({ kind, tmdbId, source })}`, signal).then((r) => r.source)
/** One level of a source: voices, seasons, episodes, files or titles to pick from. */
export const onlineBrowse = (ref: string, signal?: AbortSignal) => api.get<OnlineLevel>(`browse${qs({ ref })}`, signal)
/** Resolves a file ref and opens a proxy session; the answer is the player's play answer (mode 'stream'). */
export const onlinePlay = (body: OnlinePlayRequest) => api.post<PlayResponse>('play', body)
/** Closes the stream's proxy session (`keepalive`: the page is leaving). */
export const onlineStop = (session: string, keepalive = false): Promise<void> =>
  keepalive ? leaving('DELETE', `media/${encodeURIComponent(session)}`, {}) : api.del<void>(`media/${encodeURIComponent(session)}`)
export const onlineProgress = (kind: 'movie' | 'series', tmdbId: number, signal?: AbortSignal) =>
  api.get<OnlineProgress>(`progress${qs({ kind, tmdbId })}`, signal)
export const saveOnlineProgress = (p: OnlineProgressSave, keepalive = false): Promise<void> =>
  keepalive ? leaving('PUT', 'progress', p) : api.put<void>('progress', p)
/** Whether the way is offered: «Смотреть онлайн» is on and the sources are configured. */
export const onlineAvailable = (signal?: AbortSignal) => api.get<{ available: boolean }>('available', signal).then((r) => r.available === true)

// --- the admin's ---
/** Is the sidecar there, how many sources it lists (answers whatever the setting). */
export const onlineStatus = (signal?: AbortSignal) => api.get<OnlineStatus>('admin/status', signal)
export const onlineSettings = (signal?: AbortSignal) => api.get<OnlineSettings>('admin/settings', signal)
export const saveOnlineSettings = (enabled: boolean) => api.put<OnlineSettings>('admin/settings', { enabled })
/** «Настройки»: the Lampac address, account and waits. */
export const lampacSettings = (signal?: AbortSignal) => api.get<LampacSettings>('admin/lampac', signal)
export const saveLampacSettings = (body: LampacSave) => api.put<LampacSettings>('admin/lampac', body)
/** «Проверить»: a real, signed-in request to Lampac with these values (nothing saved). */
export const checkLampac = (body: LampacSave) => api.post<LampacCheck>('admin/lampac/check', body)
/** Lampac's sources with the admin's choice, in the order they are offered. */
export const lampacSources = (signal?: AbortSignal) => api.get<{ sources: LampacSourceRow[] }>('admin/lampac/sources', signal).then((r) => r.sources ?? [])
export const saveLampacSources = (order: string[], off: string[]) => api.put<void>('admin/lampac/sources', { order, off })
