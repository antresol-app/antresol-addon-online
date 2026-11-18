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
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
		tm := coreadapt.TMDB(ctx, core)
		titles := coreadapt.Titles{TMDB: tm, Lib: core}
		b, err := boot.Run(ctx, boot.Config{
			DataDir:         env.DataDir,
			ImportFrom:      env.Var("IMPORT_FROM"),
			LampacURL:       env.Var("LAMPAC_URL"),
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
		mux := http.NewServeMux()
		a := api.Register(mux, api.Deps{Online: b.Online, Progress: b.Progress, CacheImage: core.CacheImage,
			Enabled: b.Enabled, SetEnabled: b.SetEnabled})
		routes.set(mux)
		hd := hooks.Deps{Progress: b.Progress, DB: b.DB.DB, Users: core,
			Available: func() bool { return b.Online != nil && b.Enabled() }}
		var chx *online.ChannelExtras
		if b.Online != nil {
			// the tracking scanner asks Lampac as an adult identity no
			// profile has
			hd.Scanner = online.NewTrackingScanner(b.Online, coreadapt.Tracking{C: core}, online.User{ID: -1, Name: "tracking"}, 0)
			if tm != nil { // no TMDB: no titles outside the library
				chx = &online.ChannelExtras{Online: b.Online, TMDB: tm, Library: core}
				hd.Channels = chx
				chRoutes.set(chx)
			}
		}
		if chx == nil {
			chRoutes.set(http.NotFoundHandler())
		}
		hooks.Register(hooksMux, hd)
		ready.Set()
		<-ctx.Done()
		if chx != nil {
			chx.Close()
		}
		a.Wait()
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
