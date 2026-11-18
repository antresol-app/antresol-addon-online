# Antresol online addon

«Watch online» of Antresol as an addon (contract 1,
`docs/addons/contract.md` in the core): streaming sources for the movies and
series of a title page, the watch progress of what was played there, and the
titles outside the library that TV channels may air. It is a separate addon
with its own repository and releases, not part of the core.

## Lampac (the helper service)

The streams come from [Lampac](https://github.com/lampac-nextgen/lampac), a
helper service the manifest declares (`helpers`). Add it to the same
`compose.yml` as Antresol, under `services:`, with no `ports:`:

```yaml
  lampac:
    image: ghcr.io/lampac-nextgen/lampac:1.84.3
    restart: unless-stopped
```

and run `docker compose up -d`. The addon finds it by its service name
(`http://lampac:9118`) within half a minute, with nothing to enter, and
«Онлайн → Состояние источников» says where it is; while it is not found,
that page shows the same block and the way stays hidden.

`ANTRESOL_ONLINE_LAMPAC_URL` (`http://<host>:<port>/?uid=<id>`) wins over
the one found: use it for a Lampac that runs elsewhere, or one whose
`accsdb` needs a user id. It is never shown in the app or logged beyond its
scheme and host.

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
