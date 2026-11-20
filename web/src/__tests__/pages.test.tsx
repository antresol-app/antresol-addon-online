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

const MISSING = { configured: false, reachable: false, sources: 0, version: '', helper: { name: 'lampac', source: 'none', address: 'http://lampac:9118', compose: '  lampac:\n    image: img:1\n    restart: unless-stopped\n' } }

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

  it('a helper service not found says which one', async () => {
    serve({ enabled: false }, () => json(MISSING))
    mount(<Settings />)
    expect(await screen.findByText('Служба lampac не найдена')).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'Смотреть онлайн' })).toBeDisabled()
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
    // what Lampac is, and where the documentation says how to connect it
    expect(screen.getByText(/Онлайн-источники даёт Lampac/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Как установить и подключить Lampac' })).toHaveAttribute('href', 'https://github.com/antresol-app/antresol-addon-online/blob/main/README.ru.md#подключение-lampac')
  })

  it('a helper service not found: what to add to compose.yml', async () => {
    serve({ enabled: true }, () => json(MISSING))
    at()
    expect(await screen.findByText('Служба lampac не найдена')).toBeInTheDocument()
    expect(screen.getByText(/Добавьте этот блок в compose\.yml/)).toBeInTheDocument()
    expect(screen.getByTestId('helper-compose')).toHaveTextContent('image: img:1')
  })

  it('a helper service found: its compose address', async () => {
    serve({ enabled: true }, () => json({ ...ok, helper: { name: 'lampac', source: 'compose', address: 'http://lampac:9118' } }))
    at()
    expect(await screen.findByText('http://lampac:9118')).toBeInTheDocument()
    expect(screen.queryByTestId('helper-compose')).not.toBeInTheDocument()
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

describe('«Онлайн» → Lampac', () => {
  const LAMPAC = { url: '', composeUrl: 'http://lampac:9118', source: 'compose', server: false, authKey: '', authSet: false, authKeys: ['uid', 'account_email', 'token', 'box_mac'],
    timeout: 0, sourcesWait: 0, defaultTimeout: 20, defaultSourcesWait: 15, minWait: 5, maxWait: 120, uid: 'antresolab12cd34' }
  function serveLampac() {
    const calls: Calls = []
    let saved = { ...LAMPAC }
    let rows = [
      { id: 'alpha', name: 'Alpha', enabled: true, found: true, listed: true },
      { id: 'beta', name: 'Beta', enabled: true, found: false, listed: true },
    ]
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        const method = init?.method ?? 'GET'
        const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
        calls.push({ url, method, body })
        if (url === `${X}admin/status`) return json(ok)
        if (url === `${X}admin/settings`) return json({ enabled: true })
        if (url === `${X}admin/lampac` && method === 'GET') return json(saved)
        if (url === `${X}admin/lampac` && method === 'PUT') {
          const b = body as { url: string; authKey: string; authValue?: string; timeout: number }
          saved = { ...saved, url: b.url, authKey: b.authKey, authSet: !!b.authValue, timeout: b.timeout, source: b.url ? 'settings' : 'compose' }
          return json(saved)
        }
        if (url === `${X}admin/lampac/check`) {
          const b = body as { authValue?: string }
          return json(b.authValue === 's3cret' ? { result: 'ok', sources: 12 } : { result: 'auth', sources: 0 })
        }
        if (url === `${X}admin/lampac/sources` && method === 'GET') return json({ sources: rows })
        if (url === `${X}admin/lampac/sources` && method === 'PUT') {
          const b = body as { order: string[]; off: string[] }
          rows = b.order.map((id) => ({ ...rows.find((r) => r.id === id)!, enabled: !b.off.includes(id) }))
          return new Response(null, { status: 204 })
        }
        if (url === `${X}available`) return json({ available: true })
        return json({})
      }),
    )
    return calls
  }

  it('checks the typed sign-in for real, then saves; the sign-in is never read back', async () => {
    const calls = serveLampac()
    mount(<Settings />)
    expect(await screen.findByText('Используется служба lampac из compose.yml')).toBeInTheDocument()
    // found through compose.yml: the address is there already
    const field = screen.getByRole('textbox', { name: 'Адрес Lampac' })
    expect(field).toHaveValue('http://lampac:9118')
    await userEvent.clear(field)
    await userEvent.type(field, 'http://lampac.lan:9118')
    await userEvent.click(screen.getByRole('button', { name: /Вход/ }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: 'Идентификатор (uid)' }))
    await userEvent.type(await screen.findByLabelText('Идентификатор (uid)'), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: 'Проверить' }))
    expect(await screen.findByText(/^Авторизация не принята/)).toBeInTheDocument()
    await userEvent.clear(screen.getByLabelText('Идентификатор (uid)'))
    await userEvent.type(screen.getByLabelText('Идентификатор (uid)'), 's3cret')
    await userEvent.click(screen.getByRole('button', { name: 'Проверить' }))
    expect(await screen.findByText('Lampac отвечает, вход принят. Источников: 12')).toBeInTheDocument()
    expect(calls.filter((c) => c.url === `${X}admin/lampac` && c.method === 'PUT')).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: 'Сохранить' }))
    expect(await screen.findByText('Сохранено и применено')).toBeInTheDocument()
    const put = calls.find((c) => c.url === `${X}admin/lampac` && c.method === 'PUT')
    expect(put?.body).toEqual({ url: 'http://lampac.lan:9118', authKey: 'uid', authValue: 's3cret', timeout: 0, sourcesWait: 0 })
    expect(screen.getByText('Используется адрес отсюда')).toBeInTheDocument()
    expect(screen.getByLabelText('Идентификатор (uid)')).toHaveValue('')
    expect(screen.getByLabelText('Идентификатор (uid)')).toHaveAttribute('placeholder', 'Сохранено — введите, чтобы заменить')
  })

  it('shows the addon’s uid for Lampac’s Telegram bot with a copy button, only for a uid sign-in', async () => {
    serveLampac()
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    mount(<Settings />)
    expect(await screen.findByText('Используется служба lampac из compose.yml')).toBeInTheDocument()
    expect(screen.queryByText(/отправьте боту/)).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /Вход/ }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: 'Идентификатор (uid)' }))
    expect(await screen.findByText('Если вход в Lampac через Telegram: отправьте боту /start antresolab12cd34 и сохраните')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Копировать' }))
    expect(writeText).toHaveBeenCalledWith('antresolab12cd34')
    expect(await screen.findByRole('button', { name: 'Скопировано' })).toBeInTheDocument()
  })

  it('turns a source off at once, saving the order and the ones off', async () => {
    const calls = serveLampac()
    mount(<Settings />)
    expect(await screen.findByText('Не нашёл проверочный фильм')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('switch', { name: 'Alpha' }))
    await waitFor(() =>
      expect(calls.find((c) => c.url === `${X}admin/lampac/sources` && c.method === 'PUT')?.body).toEqual({ order: ['alpha', 'beta'], off: ['alpha'] }),
    )
    expect(screen.getByRole('switch', { name: 'Alpha' })).toHaveAttribute('aria-checked', 'false')
  })
})

describe('«Онлайн» → Lampac on the server', () => {
  it('the server’s address wins: the field is locked and says so', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url === `${X}admin/lampac`)
          return json({ url: '', source: 'override', server: true, authKey: '', authSet: false, authKeys: ['uid', 'shared_passwd'],
            timeout: 0, sourcesWait: 0, defaultTimeout: 20, defaultSourcesWait: 15, minWait: 5, maxWait: 120 })
        if (url === `${X}admin/settings`) return json({ enabled: true })
        if (url === `${X}admin/status`) return json(ok)
        if (url === `${X}admin/lampac/sources`) return json({ sources: [] })
        return json({})
      }),
    )
    mount(<Settings />)
    expect(await screen.findByText(/^Задан на сервере \(ANTRESOL_ONLINE_LAMPAC_URL\) — он главнее/)).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Адрес Lampac' })).toBeDisabled()
    expect(screen.getByRole('link', { name: 'Как подключить Lampac — три шага' })).toHaveAttribute('href', expect.stringContaining('README.ru.md#подключение-lampac'))
  })
})
