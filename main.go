// Flame Boss Hub: a local replacement for the Flame Boss / EGG Genius app.
// It talks to the grill controller on your network (no cloud), records every
// cook, serves a web page, and can hand the grill to Home Assistant.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // so the TZ setting works in the tiny Docker image
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dataDir := flag.String("data", env("FLAMEBOSS_DATA", "data"), "folder for settings and cook history")
	listen := flag.String("listen", env("FLAMEBOSS_LISTEN", "0.0.0.0:8075"), "address and port for the web page")
	health := flag.Bool("healthcheck", false, "check that a running copy answers, for Docker")
	flag.Parse()

	if *health {
		os.Exit(healthcheck(*listen))
	}

	cfg, err := LoadConfig(*dataDir)
	if err != nil {
		log.Fatalf("can't read settings in %s: %v", *dataDir, err)
	}
	store, err := OpenStore(*dataDir)
	if err != nil {
		log.Fatalf("can't open cook history in %s: %v", *dataDir, err)
	}
	defer store.Close()
	rec, err := NewRecorder(store)
	if err != nil {
		log.Fatal(err)
	}

	ctrl := NewController()
	mirror := NewMirror(ctrl)
	ha := NewHADiscovery(ctrl)
	app := &App{cfg: cfg, ctrl: ctrl, store: store, rec: rec, mirror: mirror, ha: ha, subs: map[chan []byte]bool{}}
	rec.OnChange = app.push
	ctrl.AddListener(rec)
	ctrl.AddListener(mirror)
	ctrl.AddListener(ha)
	ctrl.AddListener(&Notifier{cfg: cfg})
	ctrl.AddListener(app)

	app.applyCfg = func(old, n Config) {
		if old.Host != n.Host || old.Port != n.Port || old.PIN != n.PIN || old == (Config{}) {
			ctrl.Connect(n.Host, n.Port, n.PIN)
		}
		mpin := n.MirrorPIN
		if mpin == "" {
			mpin = n.PIN
		}
		opin := old.MirrorPIN
		if opin == "" {
			opin = old.PIN
		}
		if old == (Config{}) || old.MirrorEnabled != n.MirrorEnabled || old.MirrorPort != n.MirrorPort || opin != mpin {
			port := 0
			if n.MirrorEnabled && mpin != "" {
				port = n.MirrorPort
			}
			if err := mirror.Start(port, mpin); err != nil {
				log.Printf("built-in broker couldn't start on port %d: %v", n.MirrorPort, err)
			}
		}
		if old == (Config{}) || old.HAEnabled != n.HAEnabled || old.HABroker != n.HABroker || old.HAUser != n.HAUser || old.HAPassword != n.HAPassword {
			if n.HAEnabled {
				ha.Start(n.HABroker, n.HAUser, n.HAPassword)
			} else {
				ha.Stop()
			}
		}
		app.push()
	}
	app.applyCfg(Config{}, cfg.Get())

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("can't listen on %s: %v", *listen, err)
	}
	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Flame Boss Hub %s is running on %s (data in %s)", version, *listen, *dataDir)
	if cfg.Get().Host == "" {
		log.Printf("open the web page and enter your grill controller's IP address and PIN under Settings")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	ha.Stop()
	mirror.Stop()
	ctrl.Disconnect()
}

func healthcheck(listen string) int {
	_, port, _ := net.SplitHostPort(listen)
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
