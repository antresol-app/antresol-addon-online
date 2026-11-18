// The addon's web part: it registers its contributions when the app loads
// it (adults only: the manifest's access). React, the router and the SDK are
// the app's own (the Vite plugin maps the imports); the UI is the SDK's `ui`.
//   - the way «Онлайн» of a title's «Смотреть ▾» (its picker as the sheet),
//     offered when the server has the sources on (`useAvailable`, asked as
//     soon as this bundle loads);
//   - the full-window player route, the admin's settings and status pages and
//     their «Управление» tile;
//   - the episode guide's online sources (their name, place and player);
//   - a panel that draws nothing: it warms a title page's sources and sends
//     the positions the server could not take earlier.
// Every label and title is a key of `strings`, resolved by the core when it
// draws it, in the interface language.
import { registerAddon } from '@antresol/addon-sdk'
import { ID } from './api'
import { useOnlineEpisodePlay } from './episodes'
import { OnlinePlayer } from './OnlinePlayer'
import { Settings } from './pages/Settings'
import { Status } from './pages/Status'
import { flat, type Key } from './strings'
import { prefetchAvailable, runWay, useAvailable, WarmSources } from './ways'

// a strings key, checked here; the core resolves it
const k = (key: Key): string => key

registerAddon(ID, {
  routes: [
    // «Смотреть онлайн» over the whole window, like the core's players
    { path: 'play/:kind/:tmdbId', fullWindow: true, title: k('common.watchOnline'), element: () => <OnlinePlayer /> },
    { path: 'settings', admin: true, title: k('settings.title'), element: () => <Settings /> },
    { path: 'status', admin: true, title: k('status.title'), element: () => <Status /> },
  ],
  manageTiles: [{ label: k('settings.title'), icon: 'globe', tint: 'teal', group: 'downloads', to: 'settings', admin: true }],
  // the way id is `online:way` (`<addonId>:<label>`): what a remembered way and
  // `?way=` name; `run` returns the picker the app draws until it closes
  itemActions: [{ label: k('way'), icon: 'play', watchWay: true, useAvailable, run: runWay }],
  // under the action row of an item's and a title's page: draws nothing
  itemPanels: [{ element: WarmSources }],
  // the guide's online sources: their name, their place on the core's scale
  // (library 0 < pinned 10 < online 20 < torrent 30) and their player
  episodeActions: [{ kinds: ['online'], labels: { online: k('kind') }, order: { online: 20 }, use: useOnlineEpisodePlay }],
  strings: flat,
})

// the answer «is the way on» is there before a title page asks
void prefetchAvailable()
