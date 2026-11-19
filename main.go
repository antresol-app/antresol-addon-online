// The «Онлайн» addon (contract 1, docs/addons/contract.md in the core):
// «Смотреть онлайн» for movies and series, the watch progress of what was
// played, and the titles outside the library that TV channels may air. The
// server starts this program with its settings in ANTRESOL_* variables; it
// serves HTTP on the Unix socket ANTRESOL_ADDON_SOCKET and must answer
// GET /health within 15 s. The app reaches /api/… through the core's proxy
// (/api/addons/<id>/x/…); the program reaches the core through
// addonkit.Client, and the core calls its hooks (/hooks/<name>).
package main

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/api"
	"github.com/antresol-app/antresol-addon-online/internal/boot"
	"github.com/antresol-app/antresol-addon-online/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-online/internal/hooks"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	_ "github.com/antresol-app/antresol-addon-online/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// manifestJSON is the addon's own manifest: its helper services.
//
//go:embed antresol-addon.json
var manifestJSON []byte

// lampacHelper is the watch over the Lampac helper service the manifest
// declares (nil: none declared).
func lampacHelper(manifest []byte) (*addonkit.HelperWatch, error) {
	hs, err := addonkit.HelpersFromManifest(manifest)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.Name == "lampac" {
			return &addonkit.HelperWatch{Helper: h, OnChange: func(found bool) {
				log.Printf("online: Lampac at %s: found %v", h.Address(), found)
			}}, nil
		}
	}
	return nil, nil
}

func main() {
	env, err := addonkit.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(env.DataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	os.Remove(env.Socket) // a stale socket of an earlier run
	ln, err := net.Listen("unix", env.Socket)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	ready := &addonkit.Ready{}
	core := addonkit.NewClient(env)
	h, hooksMux := newHandler(env, core, ready)
	// the app's routes (/api/…, through the core's proxy): «Смотреть
	// онлайн», the stream proxy, the progress, available and admin/*;
	// mounted once the services are built
	routes := &lateHandler{}
	// the «ТВ» channels' streams (channels.resolve's paths), read by the
	// core's relay; set with the services
	chRoutes := &lateHandler{}
	root := http.NewServeMux()
	root.Handle("/api/", routes)
	root.Handle(online.ChannelPrefix, chRoutes)
	root.Handle("/", h)
	srv := &http.Server{Handler: root, ReadHeaderTimeout: 10 * time.Second}

	// the core stops an addon with SIGTERM and kills it 10 s later
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	// The addon's own database, the one-time import and the services come
	// up while /health already answers {"ok": true, "ready": false}; the
	// core API accepts the token meanwhile. The app's routes are mounted
	// once they are up, and then the addon is ready. A failure is logged
	// and leaves the addon not ready until the next start (which tries
	// again).
	go func() {
		helper, err := lampacHelper(manifestJSON)
		if err != nil {
			log.Printf("online: manifest helpers: %v", err)
		}
		tm := coreadapt.TMDB(ctx, core)
		titles := coreadapt.Titles{TMDB: tm, Lib: core}
		b, err := boot.Run(ctx, boot.Config{
			DataDir:    env.DataDir,
			ImportFrom: env.Var("IMPORT_FROM"),
			// the admin's address; one saved in «Настройки» wins over it
			LampacURL:       env.Var("LAMPAC_URL"),
			Helper:          helper,
			FFmpeg:          coreFFmpeg(ctx, core),
			Users:           core,
			Library:         core,
			LibraryProgress: core,
			Titles:          titles.Title,
		})
		if err != nil {
			routes.fail() // boot.Run logged why
			chRoutes.fail()
			return
		}
		if helper != nil {
			// a helper started later is found without a restart (asked
			// even while an address of the admin's is in force: clearing
			// it falls back to the helper)
			go helper.Run(ctx)
		}
		// mount puts the routes and hooks on svc (the service in force);
		// a change of the Lampac settings mounts them again on the new one
		var (
			mmu sync.Mutex
			a   *api.API
			chx *online.ChannelExtras
		)
		mount := func(svc *online.Service) {
			mmu.Lock()
			defer mmu.Unlock()
			mux := http.NewServeMux()
			a = api.Register(mux, api.Deps{Online: svc, Progress: b.Progress, CacheImage: core.CacheImage,
				Enabled: b.Enabled, SetEnabled: b.SetEnabled, Usable: b.Usable, Helper: b.HelperStatus, Lampac: b})
			routes.set(mux)
			hd := hooks.Deps{Progress: b.Progress, DB: b.DB.DB, Users: core,
				Available: func() bool { return b.Usable() && b.Enabled() }, Helpers: b.HelpersStatus}
			old := chx
			chx = nil
			if svc != nil {
				// the tracking scanner asks Lampac as an adult identity no
				// profile has
				hd.Scanner = online.NewTrackingScanner(svc, coreadapt.Tracking{C: core}, online.User{ID: -1, Name: "tracking"}, 0)
				if tm != nil { // no TMDB: no titles outside the library
					chx = &online.ChannelExtras{Online: svc, TMDB: tm, Library: core}
					hd.Channels = chx
					chRoutes.set(chx)
				}
			}
			if chx == nil {
				chRoutes.set(http.NotFoundHandler())
			}
			hooks.Register(hooksMux, hd)
			if old != nil {
				old.Close()
			}
		}
		b.SetOnChange(mount)
		ready.Set()
		<-ctx.Done()
		mmu.Lock()
		if chx != nil {
			chx.Close()
		}
		a.Wait()
		mmu.Unlock()
		b.Close()
	}()
	log.Printf("%s: serving on %s", env.ID, env.Socket)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

// infoBudget bounds the start's GET /info: a core that does not answer
// leaves the subtitles on the ffmpeg of PATH, never the start.
const infoBudget = 10 * time.Second

// coreFFmpeg is the core's ffmpeg (GET /info), for the subtitles'
// conversion; "" (ffmpeg on PATH) when the core does not say.
func coreFFmpeg(ctx context.Context, core *addonkit.Client) string {
	ctx, cancel := context.WithTimeout(ctx, infoBudget)
	defer cancel()
	info, err := core.Info(ctx)
	if err != nil {
		log.Printf("online: core info: %v (subtitles use ffmpeg on PATH)", err)
		return ""
	}
	return info.FFmpeg
}
