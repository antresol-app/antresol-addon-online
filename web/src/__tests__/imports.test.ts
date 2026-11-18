// The addon's sources reach the app only through the SDK (`@antresol/addon-sdk`),
// the app's shared React and router, and their own files: no import climbs out
// of src/, none names a core module, and CSS is CSS modules only.
import { describe, expect, it } from 'vitest'

// every source of the addon (not the tests), as text
const sources = import.meta.glob(['../*.{ts,tsx}', '../pages/*.{ts,tsx}'], { query: '?raw', import: 'default', eager: true }) as Record<string, string>

const specifiers = (text: string): string[] => [...text.matchAll(/(?:^|\n)\s*(?:import|export)\b[^;'"]*?from\s+['"]([^'"]+)['"]|(?:^|\n)\s*import\s+['"]([^'"]+)['"]|import\(\s*['"]([^'"]+)['"]\s*\)/g)].map((m) => (m[1] ?? m[2] ?? m[3])!)

const BARE = new Set(['@antresol/addon-sdk', 'react', 'react/jsx-runtime', 'react-router'])

describe('the addon’s imports', () => {
  it('finds the addon’s sources', () => {
    const names = Object.keys(sources).map((p) => p.replace('../', ''))
    expect(names).toEqual(expect.arrayContaining(['index.tsx', 'api.ts', 'ways.tsx', 'OnlinePlayer.tsx', 'OnlineSheet.tsx', 'pages/Settings.tsx', 'pages/Status.tsx']))
  })

  it('import only the SDK, the app’s React and router, and files inside src/', () => {
    const bad: string[] = []
    for (const [path, text] of Object.entries(sources)) {
      const file = path.replace('../', '')
      const depth = file.split('/').length - 1
      for (const spec of specifiers(text)) {
        if (BARE.has(spec)) continue
        if (spec.startsWith('./') || spec.startsWith('../')) {
          // how many folders it climbs; src/ itself is the limit
          const up = spec.split('/').filter((s) => s === '..').length
          if (up <= depth) continue
        }
        bad.push(`${file}: ${spec}`)
      }
    }
    expect(bad).toEqual([])
  })

  it('name no core module, test alias or other addon in any import path', () => {
    for (const [path, text] of Object.entries(sources)) {
      expect(text, path).not.toMatch(/from\s+['"][^'"]*(web\/src|test-core|test-registry|test-addon)/)
    }
  })

  it('style with CSS modules only: every stylesheet import is a *.module.css', () => {
    for (const [path, text] of Object.entries(sources)) {
      for (const spec of specifiers(text)) if (spec.endsWith('.css')) expect(spec, path).toMatch(/\.module\.css$/)
    }
  })
})
