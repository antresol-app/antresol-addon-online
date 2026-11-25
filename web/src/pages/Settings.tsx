// «Онлайн» (admins): the switch of «Смотреть онлайн» and the line of the
// sources' status under it, which opens the status page. The status says
// whether the server has online sources configured, whatever the switch; it
// is read once, so saving the switch never races it. Without a
// configuration the switch cannot be turned on, but a stored «on» can still
// be turned off.
import { ApiError, ui } from '@antresol/addon-sdk'
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router'
import { onlineSettings, onlineStatus, saveOnlineSettings } from '../api'
import { lang, useT, type TFunction } from '../i18n'
import { refreshAvailable } from '../ways'
import { setTitle } from '../title'
import type { OnlineStatus } from '../types'
import { LampacConnection, LampacSources } from './LampacSettings'
import m from './Manage.module.css'

const { GroupedSection, ListLinkRow, ListRow, Spinner, Toggle } = ui

/** The documentation's three-step guide to connecting Lampac (running it, the address and sign-in, the check), in the page's language. */
export const lampacDocs = () =>
  lang() === 'en'
    ? 'https://github.com/antresol-app/antresol-addon-online/blob/main/README.md#connecting-lampac'
    : 'https://github.com/antresol-app/antresol-addon-online/blob/main/README.ru.md#подключение-lampac'

/** What Lampac is and where to read how to connect it (under the switch, whatever the status). */
export function LampacAbout({ t }: { t: TFunction }) {
  return (
    <>
      {t('settings.online.about')}{' '}
      <a href={lampacDocs()} target="_blank" rel="noreferrer">
        {t('settings.online.docs')}
      </a>
    </>
  )
}

const errorText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback)

/** What x/admin/status says, as the one status line (empty: not known). */
export function statusLine(st: OnlineStatus | null, t: TFunction): string {
  if (!st) return ''
  if (!st.configured && st.helper?.source === 'none' && st.helper.name) return t('settings.online.helperMissing', { name: st.helper.name })
  if (!st.configured) return t('settings.online.notConfigured')
  if (!st.reachable) return t('settings.online.unreachable')
  return t('settings.online.sources', { n: st.sources })
}

export function Settings() {
  const t = useT()
  const navigate = useNavigate()
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [status, setStatus] = useState<OnlineStatus | null>(null)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  // a saved connection asks Lampac's sources again
  const [reload, setReload] = useState(0)

  useEffect(() => {
    setTitle(t('settings.title'))
    const ac = new AbortController()
    onlineSettings(ac.signal).then(
      (s) => setEnabled(s.enabled),
      (e: unknown) => !ac.signal.aborted && setError(errorText(e, t('settings.loadFailed'))),
    )
    onlineStatus(ac.signal).then(setStatus, () => {})
    return () => ac.abort()
  }, [])

  const change = (on: boolean) => {
    const before = enabled
    setEnabled(on)
    setSaving(true)
    setError('')
    saveOnlineSettings(on).then(
      (s) => {
        setEnabled(s.enabled)
        // the way's availability follows the setting at once
        void refreshAvailable()
      },
      (e: unknown) => {
        setEnabled(before)
        setError(errorText(e, t('settings.saveFailed')))
      },
    ).finally(() => setSaving(false))
  }

  if (enabled === null) {
    return (
      <section>
        <h2 className={m.title}>{t('settings.title')}</h2>
        {error ? (
          <div className={`${m.problem} ${m.afterTitle}`} role="alert">
            {error}
          </div>
        ) : (
          <div className={m.spinnerRow} role="status">
            <Spinner track className={m.spinner} /> {t('common.loading')}
          </div>
        )}
      </section>
    )
  }
  const notConfigured = status !== null && !status.configured
  return (
    <section aria-labelledby="online-settings-title">
      <header className={m.header}>
        <h2 id="online-settings-title" className={m.title}>
          {t('settings.title')}
        </h2>
      </header>
      {error && (
        <div className={m.problem} role="alert">
          {error}
        </div>
      )}
      <GroupedSection footer={<LampacAbout t={t} />}>
        <ListRow label={t('common.watchOnline')} hint={t('settings.online.hint')} status={statusLine(status, t)}>
          <Toggle label={t('common.watchOnline')} checked={enabled} disabled={saving || (notConfigured && !enabled)} onChange={change} />
        </ListRow>
        <ListLinkRow label={t('status.title')} icon="globe" onOpen={() => navigate('/addons/online/status')} />
      </GroupedSection>
      <LampacConnection
        onSaved={() => {
          setReload((n) => n + 1)
          onlineStatus().then(setStatus, () => {})
          void refreshAvailable()
        }}
      />
      <LampacSources reload={reload} />
    </section>
  )
}
