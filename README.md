# Antresol online addon

[Русский](README.ru.md)

Watch films and series without downloading them first: the title page of
[Antresol](https://antresol-app.github.io/antresol/) gets a **Watch ▾ → Online**
way, with sources, voice-overs and episodes, and the progress is saved as for
the library. Adults only.

## Install

1. In Antresol: **Settings → Server → Extensions → Add Extension**, paste
   `https://github.com/antresol-app/antresol-addon-online`, **Check**,
   **Install**.
2. Add Lampac, the helper service (below).
3. Check: **Settings → Server → Online → Source Status** says the service
   answers.

## Lampac (the helper service)

The streams come from [Lampac](https://github.com/lampac-nextgen/lampac), a
program that runs next to the server. Add it to the same `compose.yml` as
Antresol, under `services:`, next to `antresol`, with no `ports:`:

```yaml
services:
  antresol:
    # ... as before

  lampac:
    image: ghcr.io/lampac-nextgen/lampac:1.84.3
    restart: unless-stopped
```

Then run `docker compose up -d`. Within half a minute the addon finds it at
`http://lampac:9118` by its service name: nothing to enter, no restart.
**Settings → Server → Online** shows the state; while Lampac is not found, the
**Source Status** page shows this same block and the Online way stays hidden.

### Lampac elsewhere

If Lampac already runs elsewhere on your network, or its `accsdb` needs a
user id, set the server's variable (in Antresol's `.env`, then
`docker compose up -d`):

```sh
ANTRESOL_ONLINE_LAMPAC_URL=http://<host>:9118/?uid=<id>
```

It wins over the one found. It is never shown in the app or logged beyond its
scheme and host. Use it too when Antresol does not run in Docker.

## Where it shows up

- A film's or series' page: **Watch ▾ → Online**: pick a source and a
  voice-over; series get their seasons and episodes.
- **Settings → Server → Online**: turn the way on or off, and the source status.
- TV channels can air titles found online between library titles.

## Layout

| Path | What |
| --- | --- |
| `antresol-addon.json` | the manifest (`access: "adults"`, the hooks below, the Lampac helper; the build fills in `platforms` and `web`; `main.go` embeds it) |
| `main.go`, `addon.go` | the program on `addonkit`: `GET /health` (`addonkit.Ready`), the hooks at `/hooks/` |
| `internal/corecopy/` | core helper packages copied by `tools/corecopy.sh` from `tools/corecopy.list`; generated, never edited |
| `tools/corecopy.sh` | the copy (`make corecopy`) and its check (`--check`, run by `make test` and `TestCorecopyInSync` when there is a core checkout) |
| `tools/fenced.txt` | the files fenced with `//go:build ignore` until they are wired; `TestFence` keeps the list exact |
| `sql_guard_test.go` | `TestNoCoreTableSQL`: the addon reaches the core through `addonkit` only and queries its own database |
| `web/` | the web part on the SDK |
| `tools/manifest` | writes the release's `antresol-addon.json` and checks the archives |

## Hooks

`library.afterScan`, `home.continue`, `tracking.scan:online`,
`tracking.watched` and the four `channels.*` hooks (`ready`, `candidates`,
`resolve`, `release`). Permissions: `library.read`, `metadata`, `images`,
`users.read`, `tracking`, `progress`.

## The core copies

`tools/corecopy.list` names core files by glob, relative to the core's
`internal/`. The script copies the non-test ones, drops a leading
`//go:build sources`, rewrites `github.com/antresol-app/antresol/internal/X`
to `github.com/antresol-app/antresol-addon-online/internal/corecopy/X`
and refuses a file that imports a core package not copied. After a change
in the core: `make corecopy`, then `make test`.

## Build

```sh
make test                                  # go vet, go test, corecopy --check
make corecopy                              # internal/corecopy/ anew
make build                                 # dist/
make local                                 # dist-local/online/
```

The program builds on `addonkit`, the module `go.mod` requires. The web
part builds on the SDK package `@antresol/addon-sdk`:
`ANTRESOL_ADDON_SDK=<its packed .tgz>`, else one packed from the core
checkout `ANTRESOL_CORE` (its `web/` must have run `npm ci`). The copied
helpers come from that checkout too (`make corecopy`); without one,
`make test` skips `corecopy --check`. `ANTRESOL_CORE` defaults to the core
repository this tree sits in, if any.

## Licence

Licence: AGPL-3.0-or-later (it includes copies of Antresol core files)

The GNU Affero General Public License, version 3 or later (see `LICENSE`).
