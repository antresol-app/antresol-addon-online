import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('../api', async (orig) => ({
  ...(await orig<typeof import('../api')>()),
  onlineProgress: vi.fn(),
  onlineSource: vi.fn(),
  onlineSources: vi.fn(),
  onlineBrowse: vi.fn(),
}))

import { ApiError } from '@antresol/addon-sdk'
import { onlineBrowse, onlineProgress, onlineSource, onlineSources } from '../api'
import { resumeChoice } from '../OnlinePlayer'
import type { OnlineLevel } from '../types'

const film: OnlineLevel = { level: 'movie', files: [{ name: 'Дубляж', voice: 'Дубляж', qualities: ['1080p', '720p'], ref: 'file-1', subtitles: 0 }] }

function remembered() {
  vi.mocked(onlineProgress).mockResolvedValue({
    entries: [],
    last: { source: 'alpha', voice: 'Дубляж', quality: '720p', season: 0, episode: 0 },
  })
}

afterEach(() => vi.clearAllMocks())

describe('resumeChoice', () => {
  it('asks for the remembered source alone, not the whole list', async () => {
    remembered()
    vi.mocked(onlineSource).mockResolvedValue({ id: 'alpha', name: 'alpha', quality: '', ref: 'src-direct' })
    vi.mocked(onlineBrowse).mockResolvedValue(film)
    const st = await resumeChoice('movie', 7, 0, 0)
    expect(onlineSource).toHaveBeenCalledWith('movie', 7, 'alpha', undefined)
    expect(onlineBrowse).toHaveBeenCalledWith('src-direct', undefined)
    expect(onlineSources).not.toHaveBeenCalled()
    expect(st).toMatchObject({ ref: 'file-1', quality: '720p', source: 'alpha', voice: 'Дубляж' })
  })

  it('falls back to the list when that source does not answer', async () => {
    remembered()
    vi.mocked(onlineSource).mockResolvedValue({ id: 'alpha', name: 'alpha', quality: '', ref: 'src-direct' })
    vi.mocked(onlineSources).mockResolvedValue([{ id: 'alpha', name: 'Альфа', quality: '1080p', ref: 'src-listed' }])
    vi.mocked(onlineBrowse).mockImplementation(async (ref) => {
      if (ref === 'src-direct') throw new ApiError(502, 'Источник не ответил')
      return film
    })
    const st = await resumeChoice('movie', 7, 0, 0)
    expect(onlineBrowse).toHaveBeenLastCalledWith('src-listed', undefined)
    expect(st.ref).toBe('file-1')
  })

  it('throws when the list has no such source either', async () => {
    remembered()
    vi.mocked(onlineSource).mockRejectedValue(new ApiError(404, 'Ничего не нашлось'))
    vi.mocked(onlineSources).mockResolvedValue([{ id: 'beta', name: 'beta', quality: '', ref: 'b' }])
    await expect(resumeChoice('movie', 7, 0, 0)).rejects.toThrow('source gone')
    expect(onlineBrowse).not.toHaveBeenCalled()
  })

  it('an aborted resume does not ask for the list', async () => {
    remembered()
    const ac = new AbortController()
    vi.mocked(onlineSource).mockImplementation(async () => {
      ac.abort()
      throw new DOMException('aborted', 'AbortError')
    })
    await expect(resumeChoice('movie', 7, 0, 0, ac.signal)).rejects.toThrow()
    expect(onlineSources).not.toHaveBeenCalled()
  })
})
