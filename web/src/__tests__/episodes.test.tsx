// The online episode action in the core's episode guide: the guide's slot
// accepts the kind (it is not the core's), names and places it as the entry
// says, and calls the entry's hook.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { resetAddonsForTests } from '@antresol/test-registry'
import { useSourceKinds } from '@antresol/test-core/addons/slots'
import { EpisodeGuide } from '@antresol/test-core/components/episodes/EpisodeGuide'
import { ep, guide, online } from '@antresol/test-core/components/episodes/__tests__/fixtures'
import { session } from '@antresol/test-core/state/session'
import { resetAvailableForTests } from '../ways'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const adult = { id: 2, name: 'Test', avatar: 'grad-1', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
let urls: string[] = []

function serve() {
  urls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      urls.push(url)
      if (url.endsWith('/x/available')) return json({ available: true })
      if (url.endsWith('/episodes')) return json(guide([ep(1, { sources: [online('alpha', 'Voice A')] }), ep(2, { sources: [online('alpha', 'Voice A')] })]))
      if (url.endsWith('/choice')) return json({ choice: null })
      return json(adult)
    }),
  )
}

function Kinds() {
  const k = useSourceKinds()
  return <output data-testid="kinds">{`${k.label('online')}|${k.order.online}|${k.label('library')}|${k.order.library}`}</output>
}

describe('the online episode action in the episode guide', () => {
  beforeAll(async () => {
    serve()
    await import('../index')
  })
  afterAll(() => resetAddonsForTests())
  beforeEach(async () => {
    session.reset()
    serve()
    await session.login(2)
  })
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    session.reset()
  })

  it('the online episode action renders in the episode guide', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    resetAvailableForTests()
    render(
      <MemoryRouter>
        <EpisodeGuide kind="series" tmdbId={5} title="Show" />
        <Kinds />
      </MemoryRouter>,
    )
    // the guide lists the episodes and their online sources
    const rows = await screen.findAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('alpha 720p')).toBeInTheDocument()
    // the kind is named and placed by the addon's entry: «Онлайн», after the library (0), at 20
    expect(screen.getByTestId('kinds')).toHaveTextContent('Онлайн|20|Диск|0')
    // the slot did not drop it as the core's own kind
    expect(warn.mock.calls.filter(([m]) => String(m).includes('episodeActions'))).toEqual([])
    // and the guide drew the entry: its hook ran (it asks whether the way is on)
    await waitFor(() => expect(urls.some((u) => u.endsWith('/api/addons/online/x/available'))).toBe(true))
  })
})
