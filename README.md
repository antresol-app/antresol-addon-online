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
2. Connect Lampac: three steps [below](#connecting-lampac).
3. Check: **Settings → Server → Online → Source Status** says the service
   answers.

## Connecting Lampac

The streams come from [Lampac](https://github.com/lampac-nextgen/lampac), a
separate program next to the server. Connecting it takes three steps; all but
the first are done in the app: **Settings → Server → Online**, the **Lampac
Connection** block.

### Step 1. Run Lampac

Skip this if Lampac already runs on your network. Otherwise add it to the same
`compose.yml` as Antresol, under `services:`, with no `ports:`, and run
`docker compose up -d`:

```yaml
services:
  antresol:
    # ... as before

  lampac:
    image: ghcr.io/lampac-nextgen/lampac:1.84.3
    restart: unless-stopped
```

The addon finds it at `http://lampac:9118` by its service name within half a
minute.

### Step 2. Address and sign-in

In **Lampac Connection**:

- **Lampac Address** is filled in already when the service is found through
  `compose.yml`. When Lampac runs elsewhere, type its address, e.g.
  `http://192.0.2.10:9118`.
- **Sign-In** is needed when Lampac has accounts on (`accsdb` in its
  `init.conf`; without it Lampac answers "access denied"). Pick how Lampac
  knows you and type the value:
  - **ID (uid)**: an id of Lampac's `accsdb.accounts` / `users.json` (what
    Lampa asks for when signing in);
  - **Account email** (`account_email`), **Token** (`token`) or **Set-top box
    MAC address** (`box_mac`) when the account is set up under one of them;
  - **Shared password** (`accsdb.shared_passwd`): the addon registers an id of
    its own with it, as Lampa does, and a new one when its time
    (`shared_daytime`) runs out.

  The value is kept in the addon's database, added to every request to Lampac
  and never shown; logs get the scheme and host only.

### Step 3. Check and save

**Check** makes a real request to Lampac with that address and sign-in and
says one of three things: **signed in** (with the number of sources),
**sign-in not accepted** (wrong value) or **Lampac isn't responding** (wrong
address, or the service is not running). Then **Save**: it applies at once,
no restart. Lampac's sources are listed below it, to turn off or reorder.

### With the server's variable

The address can also be set in Antresol's `.env` (also when Antresol does not
run in Docker), then `docker compose up -d`:

```sh
ANTRESOL_ONLINE_LAMPAC_URL=http://<host>:9118/?uid=<id>
```

The variable wins over the address set in the app; the sign-in set there is
added to it too.

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
`resolve`, `release`), and `helpers.status` (Lampac as the addon uses it,
for the server's «Состояние» page: where its address comes from, and the
outcome of its own last call, or a 10 s probe when that is stale).
Permissions: `library.read`, `metadata`, `images`,
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
