package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// Notifier pushes alerts to a phone through ntfy.
type Notifier struct{ cfg *ConfigStore }

func (n *Notifier) Alert(text string, s *State) {
	url := n.cfg.Get().NtfyURL
	if url == "" {
		return
	}
	go func() {
		req, err := http.NewRequest("POST", url, strings.NewReader(text))
		if err != nil {
			log.Printf("ntfy: %v", err)
			return
		}
		req.Header.Set("Title", "Grill")
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "fire")
		cl := &http.Client{Timeout: 10 * time.Second}
		resp, err := cl.Do(req)
		if err != nil {
			log.Printf("ntfy: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

func (n *Notifier) StateChanged(*State) {}
func (n *Notifier) Raw(int, RawMessage) {}
