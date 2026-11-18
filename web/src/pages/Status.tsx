// «Состояние источников» (admins): whether the server has online sources
// configured, whether they answer, how many there are and their version, as
// the status route says; «Проверить снова» asks again.
import { ApiError, ui } from '@antresol/addon-sdk'
import { useCallback, useEffect, useState } from 'react'
import { onlineStatus } from '../api'
import { useT } from '../i18n'
import { setTitle } from '../title'
import type { OnlineStatus } from '../types'
import m from './Manage.module.css'

const { Button, GroupedSection, ListRow } = ui

export function Status() {
  const t = useT()
  const [status, setStatus] = useState<OnlineStatus | null>(null)
  const [error, setError] = useState('')
  const [asking, setAsking] = useState(true)

  const load = useCallback(
    (signal?: AbortSignal) => {
      setAsking(true)
      setError('')
      onlineStatus(signal).then(
        (s) => {
          setStatus(s)
          setAsking(false)
        },
        (e: unknown) => {
          if (signal?.aborted) return
          setError(e instanceof ApiError ? e.message : t('common.genericError'))
          setAsking(false)
        },
      )
    },
    [t],
  )
  useEffect(() => {
    setTitle(t('status.title'))
    const ac = new AbortController()
    load(ac.signal)
    return () => ac.abort()
  }, [])

  // what the service says, in one word: not set up, not answering, or answering
  const service = !status ? '' : !status.configured ? t('settings.online.notConfigured') : !status.reachable ? t('settings.online.unreachable') : t('status.reachable')
  return (
    <section aria-labelledby="online-status-title">
      <header className={m.header}>
        <h2 id="online-status-title" className={m.title}>
          {t('status.title')}
        </h2>
      </header>
      {error && (
        <div className={m.problem} role="alert">
          {error}
        </div>
      )}
      {!status && !error && (
        <div className={m.spinnerRow} role="status">
          <span className={m.spinner} aria-hidden /> {t('common.loading')}
        </div>
      )}
      {status && (
        <GroupedSection>
          <ListRow label={t('status.service')}>
            <span className={m.value}>{service}</span>
          </ListRow>
          {status.configured && status.reachable && (
            <ListRow label={t('status.sources')}>
              <span className={m.value}>{status.sources}</span>
            </ListRow>
          )}
          {status.version && (
            <ListRow label={t('status.version')}>
              <span className={m.value}>{status.version}</span>
            </ListRow>
          )}
        </GroupedSection>
      )}
      <div className={m.actions}>
        <Button variant="secondary" size="small" icon="restart" disabled={asking} onClick={() => load()}>
          {t('status.recheck')}
        </Button>
      </div>
    </section>
  )
}
