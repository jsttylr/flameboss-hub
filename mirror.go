package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Mirror is a small MQTT broker that looks like a Flame Boss controller, so the
// Home Assistant integration (or the official app's local mode, or any other
// tool) can connect here instead of to the grill. The grill then only ever sees
// one local connection: this app.
type Mirror struct {
	mu     sync.Mutex
	server *mochi.Server
	ctrl   *Controller
	port   int
}

func NewMirror(ctrl *Controller) *Mirror { return &Mirror{ctrl: ctrl} }

// Start (re)starts the broker. port 0 stops it.
func (m *Mirror) Start(port int, pin string) error {
	m.Stop()
	if port == 0 {
		return nil
	}
	s := mochi.New(&mochi.Options{InlineClient: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rules := auth.AuthRules{{Username: auth.RString("fb"), Password: auth.RString(pin), Allow: true}}
	if err := s.AddHook(new(auth.Hook), &auth.Options{Ledger: &auth.Ledger{Auth: rules}}); err != nil {
		return err
	}
	if err := s.AddHook(&replayHook{m: m}, nil); err != nil {
		return err
	}
	if err := s.AddListener(listeners.NewTCP(listeners.Config{ID: "fb", Address: fmt.Sprintf(":%d", port)})); err != nil {
		return err
	}
	// Commands from clients go on to the real controller.
	// (mochi passes its own inline client to this handler, not the sender.)
	if err := s.Subscribe("flameboss/+/recv", 1, func(_ *mochi.Client, _ packets.Subscription, pk packets.Packet) {
		parts := strings.Split(pk.TopicName, "/")
		id, _ := strconv.Atoi(parts[1])
		if err := m.ctrl.SendRaw(id, pk.Payload); err != nil {
			log.Printf("couldn't pass a command on to the grill: %v", err)
		}
	}); err != nil {
		return err
	}
	if err := s.Serve(); err != nil {
		return err
	}
	m.mu.Lock()
	m.server, m.port = s, port
	m.mu.Unlock()
	log.Printf("built-in broker for Home Assistant listening on port %d (user fb)", port)
	return nil
}

func (m *Mirror) Stop() {
	m.mu.Lock()
	s := m.server
	m.server = nil
	m.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (m *Mirror) publish(deviceID int, msg RawMessage) {
	m.mu.Lock()
	s := m.server
	m.mu.Unlock()
	if s == nil || deviceID == 0 {
		return
	}
	s.Publish(fmt.Sprintf("flameboss/%d/%s", deviceID, msg.Topic), msg.Payload, false, 1)
}

// Listener interface: forward every controller message.
func (m *Mirror) Raw(deviceID int, msg RawMessage) { m.publish(deviceID, msg) }
func (m *Mirror) StateChanged(*State)              {}
func (m *Mirror) Alert(string, *State)             {}

// replayHook sends the full dump to anyone who subscribes, like the real controller.
type replayHook struct {
	mochi.HookBase
	m *Mirror
}

func (h *replayHook) ID() string { return "flameboss-replay" }

func (h *replayHook) Provides(b byte) bool {
	return bytes.Contains([]byte{mochi.OnSubscribed}, []byte{b})
}

func (h *replayHook) OnSubscribed(cl *mochi.Client, pk packets.Packet, codes []byte) {
	if cl.Net.Inline {
		return
	}
	id, dump := h.m.ctrl.Dump()
	go func() {
		for _, msg := range dump {
			h.m.publish(id, msg)
		}
	}()
}
