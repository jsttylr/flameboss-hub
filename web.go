package main

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// App ties the pieces together for the web page.
type App struct {
	cfg      *ConfigStore
	ctrl     *Controller
	store    *Store
	rec      *Recorder
	mirror   *Mirror
	ha       *HADiscovery
	applyCfg func(old, new Config)

	subsMu sync.Mutex
	subs   map[chan []byte]bool
}

// broadcast tells every open page about a change (Listener interface).
func (a *App) StateChanged(*State)   { a.push() }
func (a *App) Alert(string, *State)  { a.push() }
func (a *App) Raw(int, RawMessage)   {}

func (a *App) push() {
	b, _ := json.Marshal(a.statusPayload())
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- b:
		default: // slow client: skip this update
		}
	}
}

func (a *App) statusPayload() map[string]any {
	host, connected, lastErr, authFail := a.ctrl.Status()
	out := map[string]any{
		"state": a.ctrl.Snapshot(),
		"units": a.cfg.Get().Units,
		"controller": map[string]any{
			"host": host, "connected": connected, "error": lastErr, "auth_failed": authFail,
		},
	}
	if id := a.rec.Current(); id != 0 {
		if c, err := a.store.Cook(id); err == nil && c != nil {
			out["cook"] = c
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.statusPayload()) })

	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan []byte, 8)
		a.subsMu.Lock()
		a.subs[ch] = true
		a.subsMu.Unlock()
		defer func() {
			a.subsMu.Lock()
			delete(a.subs, ch)
			a.subsMu.Unlock()
		}()
		b, _ := json.Marshal(a.statusPayload())
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		tick := time.NewTicker(25 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-tick.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	})

	mux.HandleFunc("POST /api/set_temp", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Celsius float64 `json:"celsius"`
		}
		if err := readJSON(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		s := a.ctrl.Snapshot()
		if body.Celsius < s.MinTemp-0.1 || body.Celsius > s.MaxTemp+0.1 {
			fail(w, 400, fmt.Errorf("the controller allows %.0f to %.0f °C", s.MinTemp, s.MaxTemp))
			return
		}
		if err := a.ctrl.SetTemp(body.Celsius); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/silence", func(w http.ResponseWriter, r *http.Request) {
		if err := a.ctrl.Silence(); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/cook/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		readJSON(r, &body)
		id, err := a.rec.Start(body.Name, a.ctrl.Snapshot().Labels)
		if err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, map[string]int64{"id": id})
	})
	mux.HandleFunc("POST /api/cook/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := a.rec.Stop(); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/cook/note", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := readJSON(r, &body); err != nil || body.Text == "" {
			fail(w, 400, fmt.Errorf("write a note first"))
			return
		}
		if err := a.rec.AddNote(body.Text); err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/cooks", func(w http.ResponseWriter, r *http.Request) {
		cooks, err := a.store.Cooks(500)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if cooks == nil {
			cooks = []Cook{}
		}
		writeJSON(w, cooks)
	})
	cookID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, 400, err)
			return 0, false
		}
		return id, true
	}
	mux.HandleFunc("GET /api/cooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cookID(w, r)
		if !ok {
			return
		}
		c, err := a.store.Cook(id)
		if err != nil || c == nil {
			fail(w, 404, fmt.Errorf("no such cook"))
			return
		}
		samples, _ := a.store.Samples(id)
		events, _ := a.store.Events(id)
		writeJSON(w, map[string]any{"cook": c, "samples": samples, "events": events})
	})
	mux.HandleFunc("PATCH /api/cooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cookID(w, r)
		if !ok {
			return
		}
		var body struct{ Name, Notes string }
		if err := readJSON(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		if err := a.store.UpdateCook(id, body.Name, body.Notes); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/cooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cookID(w, r)
		if !ok {
			return
		}
		if id == a.rec.Current() {
			a.rec.Stop()
		}
		if err := a.store.DeleteCook(id); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/cooks/{id}/csv", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cookID(w, r)
		if !ok {
			return
		}
		samples, err := a.store.Samples(id)
		if err != nil {
			fail(w, 500, err)
			return
		}
		f := a.cfg.Get().Units == "F"
		conv := func(t Temp) string {
			if t == nil {
				return ""
			}
			v := *t
			if f {
				v = v*9/5 + 32
			}
			return strconv.FormatFloat(v, 'f', 1, 64)
		}
		u := map[bool]string{true: "F", false: "C"}[f]
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cook-%d.csv"`, id))
		cw := csv.NewWriter(w)
		cw.Write([]string{"time", "pit_" + u, "meat1_" + u, "meat2_" + u, "meat3_" + u, "set_" + u, "blower_pct"})
		for _, s := range samples {
			b := ""
			if s.Blower != nil {
				b = strconv.FormatFloat(*s.Blower, 'f', 0, 64)
			}
			cw.Write([]string{s.T.Format(time.RFC3339), conv(s.Temps[0]), conv(s.Temps[1]), conv(s.Temps[2]), conv(s.Temps[3]), conv(s.SetTemp), b})
		}
		cw.Flush()
	})

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.cfg.Get().Public()) })
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		// Blank secret fields keep the saved value.
		var body struct {
			Config
			ClearMirrorPIN bool `json:"clear_mirror_pin"`
		}
		if err := readJSON(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		old := a.cfg.Get()
		n := body.Config
		if n.PIN == "" {
			n.PIN = old.PIN
		}
		if n.MirrorPIN == "" && !body.ClearMirrorPIN {
			n.MirrorPIN = old.MirrorPIN
		}
		if n.HAPassword == "" {
			n.HAPassword = old.HAPassword
		}
		if err := a.cfg.Set(n); err != nil {
			fail(w, 500, err)
			return
		}
		a.applyCfg(old, a.cfg.Get())
		writeJSON(w, a.cfg.Get().Public())
	})
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
