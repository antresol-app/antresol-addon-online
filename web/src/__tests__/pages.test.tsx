// The admin's two pages: «Онлайн» (the switch of «Смотреть онлайн» and the
// sources' status line) and «Состояние источников».
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import { Settings } from '../pages/Settings'
import { Status } from '../pages/Status'
import { resetAvailableForTests } from '../ways'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const X = '/api/addons/online/x/'
const ok = { configured: true, reachable: true, sources: 17, version: '1.2.3' }

type Calls = { url: string; method: string; body: unknown }[]
/** The addon's admin routes: `state` is the setting, `status` the status (a function: answered per call). */
function serve(state: { enabled: boolean }, status: () => Response, put: () => Response | undefined = () => undefined) {
  const calls: Calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
      calls.push({ url, method, body })
      if (url === `${X}admin/status`) return status()
      if (url === `${X}admin/settings` && method === 'GET') return json({ enabled: state.enabled })
      if (url === `${X}admin/settings` && method === 'PUT') {
        const refused = put()
        if (refused) return refused
        state.enabled = (body as { enabled: boolean }).enabled
        return json({ enabled: state.enabled })
      }
      if (url === `${X}available`) return json({ available: state.enabled })
      return json({})
    }),
  )
  return calls
}

const Where = () => <output data-testid="where">{useLocation().pathname}</output>
const mount = (ui: ReactElement) =>
  render(
    <MemoryRouter initialEntries={['/addons/online/settings']}>
      <Routes>
        <Route path="/addons/online/settings" element={ui} />
        <Route path="/addons/online/status" element={<Where />} />
      </Routes>
    </MemoryRouter>,
  )

beforeEach(() => resetAvailableForTests())
afterEach(() => vi.unstubAllGlobals())

describe('«Онлайн» (settings)', () => {
  it('shows the switch as the server has it, with the sources status line', async () => {
    serve({ enabled: true }, () => json(ok))
    mount(<Settings />)
    const sw = await screen.findByRole('switch', { name: 'Смотреть онлайн' })
    expect(sw).toHaveAttribute('aria-checked', 'true')
    expect(await screen.findByText('Источники онлайн: 17')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Онлайн' })).toBeInTheDocument()
  })

  it('saving the switch sends {enabled} to x/admin/settings and the way follows at once', async () => {
    const calls = serve({ enabled: true }, () => json(ok))
    mount(<Settings />)
    const sw = await screen.findByRole('switch', { name: 'Смотреть онлайн' })
    await userEvent.click(sw)
    await waitFor(() => expect(sw).toHaveAttribute('aria-checked', 'false'))
    expect(calls.filter((c) => c.method === 'PUT')).toEqual([{ url: `${X}admin/settings`, method: 'PUT', body: { enabled: false } }])
    // the way's availability is asked again, so a title page does not keep offering it for a minute
    await waitFor(() => expect(calls.some((c) => c.url === `${X}available`)).toBe(true))
  })

  it('a save that fails puts the switch back and says why', async () => {
    serve({ enabled: true }, () => json(ok), () => json({ error: 'Внутренняя ошибка сервера' }, 500))
    mount(<Settings />)
    const sw = await screen.findByRole('switch', { name: 'Смотреть онлайн' })
    await userEvent.click(sw)
    expect(await screen.findByRole('alert')).toHaveTextContent('Внутренняя ошибка сервера')
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('without a configuration it cannot be turned on, but a stored «on» can still be turned off', async () => {
    const calls = serve({ enabled: false }, () => json({ configured: false, reachable: false, sources: 0, version: '' }))
    const off = mount(<Settings />)
    expect(await screen.findByText('Не настроено на сервере')).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'Смотреть онлайн' })).toBeDisabled()
    off.unmount()
    serve({ enabled: true }, () => json({ configured: false, reachable: false, sources: 0, version: '' }))
    mount(<Settings />)
    await screen.findByText('Не настроено на сервере')
    expect(screen.getByRole('switch', { name: 'Смотреть онлайн' })).toBeEnabled()
    expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  })

  it('says when the status does not answer, and the row of the status page opens it', async () => {
    serve({ enabled: true }, () => json({ error: 'нет' }, 502))
    mount(<Settings />)
    await screen.findByRole('switch', { name: 'Смотреть онлайн' })
    await userEvent.click(screen.getByRole('button', { name: /Состояние источников/ }))
    expect(await screen.findByTestId('where')).toHaveTextContent('/addons/online/status')
  })

  it('a setting that cannot be read is a problem, not a switch', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'Недоступно в детском профиле' }, 403)))
    mount(<Settings />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Недоступно в детском профиле')
    expect(screen.queryByRole('switch')).not.toBeInTheDocument()
  })
})

describe('«Состояние источников»', () => {
  const at = () => render(<MemoryRouter><Status /></MemoryRouter>)

  it('answering: the service, how many sources, the version', async () => {
    serve({ enabled: true }, () => json(ok))
    at()
    expect(await screen.findByText('Отвечает')).toBeInTheDocument()
    expect(screen.getByText('17')).toBeInTheDocument()
    expect(screen.getByText('1.2.3')).toBeInTheDocument()
  })

  it('not set up, and not answering', async () => {
    serve({ enabled: true }, () => json({ configured: false, reachable: false, sources: 0, version: '' }))
    const first = at()
    expect(await screen.findByText('Не настроено на сервере')).toBeInTheDocument()
    expect(screen.queryByText('Источников')).not.toBeInTheDocument()
    first.unmount()
    serve({ enabled: true }, () => json({ configured: true, reachable: false, sources: 0, version: '' }))
    at()
    expect(await screen.findByText('Lampac не отвечает')).toBeInTheDocument()
  })

  it('a failed read is said, and «Проверить снова» asks again', async () => {
    let n = 0
    serve({ enabled: true }, () => (++n === 1 ? json({ error: 'Что-то пошло не так' }, 500) : json(ok)))
    at()
    expect(await screen.findByRole('alert')).toHaveTextContent('Что-то пошло не так')
    await userEvent.click(screen.getByRole('button', { name: 'Проверить снова' }))
    expect(await screen.findByText('Отвечает')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
