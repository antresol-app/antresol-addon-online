// The addon's strings in the interface language: t(key, vars) over its own
// catalogs (strings.ts), with {name} placeholders as the core's own t. useT
// re-renders on a language change through the SDK's useAddonT; tr is for
// code outside React (the picker's error texts, read when they are shown).
import { locale, useAddonT } from '@antresol/addon-sdk'
import { useMemo } from 'react'
import { ID } from './api'
import { catalogs, type Key } from './strings'

export type { Key }
export type Lang = 'ru' | 'en'
type Vars = Record<string, string | number>

/** The interface language, from the SDK's locale (ru-RU / en-US). */
export const lang = (): Lang => (locale().startsWith('en') ? 'en' : 'ru')

export function translate(l: Lang, key: Key, vars?: Vars): string {
  const s = catalogs[l][key] ?? catalogs.ru[key]
  if (s === undefined) return key
  return vars ? s.replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m)) : s
}

/** Outside React (or in a component that called useT). */
export const tr = (key: Key, vars?: Vars): string => translate(lang(), key, vars)

export type TFunction = (key: Key, vars?: Vars) => string

/** t bound to the interface language; a new function after a change. */
export function useT(): TFunction {
  const sdkT = useAddonT(ID) // subscribes to language changes
  return useMemo<TFunction>(() => {
    const l = lang()
    return (key, vars) => translate(l, key, vars)
  }, [sdkT])
}
