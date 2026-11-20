// «Онлайн» → «Настройки» (admins): the Lampac connection — the address
// (filled with the helper service's when found; the server's variable wins
// over it), the sign-in Lampac's accounts (accsdb) ask for, «Проверить» (a
// real signed-in request with the typed values), the waits — and Lampac's
// sources, on or off and in the order they are offered. The sign-in value
// is never read back: the field says it is saved, and a new one replaces it.
import { ApiError, ui } from '@antresol/addon-sdk'
import { useEffect, useState } from 'react'
import { checkLampac, lampacSettings, lampacSources, saveLampacSettings, saveLampacSources } from '../api'
import { useT, type TFunction } from '../i18n'
import type { LampacCheck, LampacSave, LampacSettings as Settings, LampacSourceRow } from '../types'
import { lampacDocs } from './Settings'
import m from './Manage.module.css'

const { Button, GroupedSection, ListRow, NumberField, Select, SortableList, SortableRowText, TextField, Toggle } = ui

const errorText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback)

/** Where the address in force comes from, in one line. */
function sourceLine(s: Settings, t: TFunction): string {
  switch (s.source) {
    case 'settings':
      return t('lampac.from.settings')
    case 'override':
      return t('lampac.from.server')
    case 'compose':
      return t('lampac.from.compose')
  }
  return t('lampac.from.none')
}

const authLabel = (key: string, t: TFunction): string => {
  switch (key) {
    case 'uid':
      return t('lampac.auth.uid')
    case 'account_email':
      return t('lampac.auth.email')
    case 'token':
      return t('lampac.auth.token')
    case 'box_mac':
      return t('lampac.auth.mac')
    case 'shared_passwd':
      return t('lampac.auth.shared')
  }
  return t('lampac.auth.none')
}

/**
 * Lampac's Telegram sign-in (TelegramAuthBot) takes an id the admin sends
 * the bot as /start <uid>: the addon's own id (sent with the shared
 * password; for «Идентификатор» it is the value to type), with a copy button.
 */
function TelegramUid({ uid }: { uid: string }) {
  const t = useT()
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard?.writeText(uid).then(
      () => setCopied(true),
      () => setCopied(false),
    )
  }
  return (
    <ListRow wide label={t('lampac.telegram')} hint={t('lampac.telegram.hint', { uid })}>
      <span className={m.sourceRow}>
        <code className={m.uid}>{uid}</code>
        <Button type="button" variant="secondary" size="small" onClick={copy}>
          {copied ? t('lampac.telegram.copied') : t('lampac.telegram.copy')}
        </Button>
      </span>
    </ListRow>
  )
}

export function LampacConnection({ onSaved }: { onSaved?: () => void }) {
  const t = useT()
  const [saved, setSaved] = useState<Settings | null>(null)
  const [url, setUrl] = useState('')
  const [authKey, setAuthKey] = useState('')
  const [authValue, setAuthValue] = useState('')
  const [timeout, setTimeoutSec] = useState(0)
  const [wait, setWait] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [done, setDone] = useState('')
  const [checking, setChecking] = useState(false)
  const [check, setCheck] = useState<LampacCheck | null>(null)

  const fill = (s: Settings) => {
    setSaved(s)
    // found through compose.yml: its address, ready to keep or change
    setUrl(s.url || s.composeUrl || '')
    setAuthKey(s.authKey ?? '')
    setAuthValue('')
    setTimeoutSec(s.timeout || s.defaultTimeout)
    setWait(s.sourcesWait || s.defaultSourcesWait)
  }
  useEffect(() => {
    const ac = new AbortController()
    lampacSettings(ac.signal).then(fill, (e: unknown) => !ac.signal.aborted && setError(errorText(e, t('settings.loadFailed'))))
    return () => ac.abort()
  }, [])
  if (!saved) return error ? <div className={m.problem} role="alert">{error}</div> : null

  const changed = () => {
    setError('')
    setDone('')
    setCheck(null)
  }
  // the compose address as it was found is not saved as the admin's own
  const typedUrl = url.trim() === (saved.composeUrl ?? '') && !saved.url ? '' : url.trim()
  const body = (): LampacSave => ({
    url: typedUrl,
    authKey,
    ...(authValue !== '' || !authKey ? { authValue: authKey ? authValue : '' } : {}),
    // the defaults are kept as «default», so a later default applies
    timeout: timeout === saved.defaultTimeout ? 0 : timeout,
    sourcesWait: wait === saved.defaultSourcesWait ? 0 : wait,
  })
  const runCheck = async () => {
    setChecking(true)
    changed()
    try {
      setCheck(await checkLampac(body()))
    } catch (e) {
      setError(errorText(e, t('lampac.check.failed')))
    } finally {
      setChecking(false)
    }
  }
  const save = async () => {
    setBusy(true)
    changed()
    try {
      const out = await saveLampacSettings(body())
      fill(out)
      setDone(t('lampac.saved'))
      onSaved?.()
    } catch (e) {
      setError(errorText(e, t('settings.saveFailed')))
    } finally {
      setBusy(false)
    }
  }
  const placeholder = 'http://lampac:9118'
  const checkLine = !check
    ? ''
    : check.result === 'ok'
      ? t('lampac.check.ok', { n: check.sources })
      : check.result === 'auth'
        ? t('lampac.check.auth')
        : check.result === 'down'
          ? t('lampac.check.down')
          : t('lampac.check.error')
  const authOptions = ['', ...(saved.authKeys ?? [])].map((k) => ({ value: k, label: authLabel(k, t) }))
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        void save()
      }}
    >
      <GroupedSection
        title={t('lampac.title')}
        footer={
          <>
            {t('lampac.footer')}{' '}
            <a href={lampacDocs()} target="_blank" rel="noreferrer">
              {t('lampac.guide')}
            </a>
          </>
        }
      >
        <ListRow wide label={t('lampac.url')} hint={saved.server ? t('lampac.url.server') : sourceLine(saved, t)}>
          <TextField
            label={t('lampac.url')}
            hideLabel
            disabled={saved.server}
            value={url}
            placeholder={placeholder}
            onChange={(e) => {
              setUrl(e.target.value)
              changed()
            }}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            inputMode="url"
          />
        </ListRow>
        <ListRow label={t('lampac.auth')} hint={t('lampac.auth.hint')}>
          <Select
            label={t('lampac.auth')}
            value={authKey}
            options={authOptions}
            onChange={(v) => {
              setAuthKey(v)
              changed()
            }}
          />
        </ListRow>
        {authKey && (
          <ListRow wide label={authLabel(authKey, t)}>
            <TextField
              label={authLabel(authKey, t)}
              hideLabel
              type="password"
              revealable
              autoComplete="off"
              value={authValue}
              placeholder={saved.authSet && saved.authKey === authKey ? t('lampac.auth.kept') : ''}
              onChange={(e) => {
                setAuthValue(e.target.value)
                changed()
              }}
            />
          </ListRow>
        )}
        {(authKey === 'uid' || authKey === 'shared_passwd') && saved.uid && <TelegramUid uid={saved.uid} />}
        <ListRow label={t('lampac.timeout')} hint={t('lampac.timeout.hint')}>
          <NumberField label={t('lampac.timeout')} value={timeout} min={saved.minWait} max={saved.maxWait} unit={t('lampac.sec')} chars={3}
            onChange={(v) => {
              setTimeoutSec(v)
              changed()
            }}
          />
        </ListRow>
        <ListRow label={t('lampac.wait')} hint={t('lampac.wait.hint')}>
          <NumberField label={t('lampac.wait')} value={wait} min={saved.minWait} max={saved.maxWait} unit={t('lampac.sec')} chars={3}
            onChange={(v) => {
              setWait(v)
              changed()
            }}
          />
        </ListRow>
      </GroupedSection>
      {error && (
        <div className={m.problem} role="alert">
          {error}
        </div>
      )}
      <div className={m.actions}>
        <Button type="button" variant="secondary" size="small" loading={checking} disabled={checking || busy} onClick={() => void runCheck()}>
          {t('lampac.check')}
        </Button>
        <Button type="submit" variant="secondary" size="small" loading={busy} disabled={busy || checking}>
          {t('lampac.save')}
        </Button>
      </div>
      <div className={m.subtitle} role="status">
        {checkLine || done}
      </div>
    </form>
  )
}

/** Lampac's sources: on or off, in the order «Смотреть онлайн» offers them; each change is saved at once. */
export function LampacSources({ reload }: { reload: number }) {
  const t = useT()
  const [rows, setRows] = useState<LampacSourceRow[] | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    const ac = new AbortController()
    setError('')
    lampacSources(ac.signal).then(setRows, (e: unknown) => {
      if (ac.signal.aborted) return
      setRows(null)
      setError(errorText(e, t('lampac.sources.failed')))
    })
    return () => ac.abort()
  }, [reload])
  const save = (next: LampacSourceRow[]) => {
    const before = rows
    setRows(next)
    setError('')
    saveLampacSources(
      next.map((r) => r.id),
      next.filter((r) => !r.enabled).map((r) => r.id),
    ).catch((e: unknown) => {
      setRows(before)
      setError(errorText(e, t('settings.saveFailed')))
    })
  }
  return (
    <GroupedSection title={t('lampac.sources')} footer={t('lampac.sources.footer')}>
      {error && <ListRow label={t('lampac.sources')} status={error} />}
      {rows && (
        <ListRow wide label={t('lampac.sources.list')} hint={t('lampac.sources.hint')}>
          <SortableList
            items={rows}
            getKey={(r) => r.id}
            itemLabel={(r) => r.name}
            label={t('lampac.sources')}
            moveButtons
            onChange={save}
            empty={<p className={m.subtitle}>{t('lampac.sources.none')}</p>}
            renderItem={(r) => (
              <span className={m.sourceRow}>
                <SortableRowText title={r.name} detail={!r.listed ? t('lampac.sources.gone') : r.found ? undefined : t('lampac.sources.notFound')} />
                <Toggle label={r.name} checked={r.enabled} onChange={(on) => save(rows.map((x) => (x.id === r.id ? { ...x, enabled: on } : x)))} />
              </span>
            )}
          />
        </ListRow>
      )}
    </GroupedSection>
  )
}
