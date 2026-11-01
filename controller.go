package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// RawMessage is one message from the controller, kept so the built-in broker
// can replay a full dump to new subscribers (like the real controller does).
type RawMessage struct {
	Topic   string // e.g. send/data (without flameboss/<id>/)
	Payload []byte
}

// Listener gets told about state changes and alerts.
type Listener interface {
	StateChanged(s *State)
	Alert(text string, s *State)
	Raw(deviceID int, msg RawMessage)
}

// Controller keeps one connection to a Flame Boss controller.
type Controller struct {
	mu        sync.Mutex
	state     *State
	cache     map[string]RawMessage // latest message per topic+name, for replay
	order     []string
	client    mqtt.Client
	host      string
	listeners []Listener
	lastErr   string
	authFail  bool
}

func NewController() *Controller {
	return &Controller{state: newState(), cache: map[string]RawMessage{}}
}

func (c *Controller) AddListener(l Listener) { c.listeners = append(c.listeners, l) }

// Snapshot returns a copy of the current state.
func (c *Controller) Snapshot() *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Clone()
}

// Status returns a short connection status for the settings page.
func (c *Controller) Status() (host string, connected bool, lastErr string, authFail bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host, c.state.Connected, c.lastErr, c.authFail
}

// Dump returns the cached messages in arrival order.
func (c *Controller) Dump() (int, []RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RawMessage, 0, len(c.order))
	for _, k := range c.order {
		out = append(out, c.cache[k])
	}
	return c.state.DeviceID, out
}

// Connect (re)connects using the given settings. An empty host disconnects.
func (c *Controller) Connect(host string, port int, pin string) {
	c.Disconnect()
	c.mu.Lock()
	c.host = host
	c.lastErr = ""
	c.authFail = false
	c.mu.Unlock()
	if host == "" {
		return
	}
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", host, port)).
		SetClientID(fmt.Sprintf("fb-hub-%d", time.Now().UnixNano()%1e8)).
		SetUsername("fb").
		SetPassword(pin).
		SetProtocolVersion(4).
		SetKeepAlive(5 * time.Second). // the controller gives up on quiet clients after ~10 s
		SetPingTimeout(4 * time.Second).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		SetMaxReconnectInterval(time.Minute).
		SetOrderMatters(false)
	opts.SetOnConnectHandler(func(cl mqtt.Client) {
		log.Printf("connected to the grill controller at %s", host)
		c.mu.Lock()
		c.state.Connected = true
		c.lastErr = ""
		c.authFail = false
		c.cache = map[string]RawMessage{}
		c.order = nil
		c.mu.Unlock()
		c.notify()
		// QoS 1 matters: the controller waits for acknowledgements.
		cl.Subscribe("flameboss/+/send/#", 1, c.onMessage)
	})
	opts.SetConnectionLostHandler(func(cl mqtt.Client, err error) {
		log.Printf("lost the grill controller at %s: %v", host, err)
		c.mu.Lock()
		c.state.Connected = false
		c.lastErr = err.Error()
		c.mu.Unlock()
		c.notify()
	})
	cl := mqtt.NewClient(opts)
	c.mu.Lock()
	c.client = cl
	c.mu.Unlock()
	go func() {
		tok := cl.Connect()
		tok.Wait()
		if err := tok.Error(); err != nil {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.authFail = strings.Contains(strings.ToLower(err.Error()), "not authorized") ||
				strings.Contains(strings.ToLower(err.Error()), "bad user name")
			c.mu.Unlock()
			log.Printf("can't connect to the grill controller at %s: %v", host, err)
		}
	}()
}

func (c *Controller) Disconnect() {
	c.mu.Lock()
	cl := c.client
	c.client = nil
	c.state.Connected = false
	c.mu.Unlock()
	if cl != nil {
		cl.Disconnect(200)
	}
}

func (c *Controller) onMessage(_ mqtt.Client, m mqtt.Message) {
	parts := strings.SplitN(m.Topic(), "/", 3) // flameboss, <id>, send/...
	msg := decodeMessage(m.Payload())
	if msg == nil || len(parts) < 3 {
		return
	}
	id, _ := strconv.Atoi(parts[1])
	now := time.Now()
	raw := RawMessage{Topic: parts[2], Payload: append([]byte(nil), m.Payload()...)}

	c.mu.Lock()
	if c.state.DeviceID == 0 {
		c.state.DeviceID = id
	}
	alert := c.state.Apply(msg, now)
	name, _ := msg["name"].(string)
	key := raw.Topic + "|" + name
	if _, seen := c.cache[key]; !seen {
		c.order = append(c.order, key)
	}
	if name == "meat_alarm" { // one per probe
		key = fmt.Sprintf("%s|%v", key, msg["sensor"])
		if _, seen := c.cache[key]; !seen {
			c.order = append(c.order, key)
		}
	}
	c.cache[key] = raw
	snap := c.state.Clone()
	c.mu.Unlock()

	for _, l := range c.listeners {
		l.Raw(id, raw)
		l.StateChanged(snap)
		if alert != "" {
			l.Alert(alert, snap)
		}
	}
}

func (c *Controller) notify() {
	snap := c.Snapshot()
	for _, l := range c.listeners {
		l.StateChanged(snap)
	}
}

// Send publishes one command to the controller.
func (c *Controller) Send(cmd map[string]any) error {
	c.mu.Lock()
	cl, id, ok := c.client, c.state.DeviceID, c.state.Connected
	c.mu.Unlock()
	if cl == nil || !ok {
		return errors.New("not connected to the grill controller")
	}
	if id == 0 {
		return errors.New("the controller hasn't reported its ID yet")
	}
	b, _ := json.Marshal(cmd)
	return c.SendRaw(id, b)
}

// SendRaw forwards an already-encoded command (used by the built-in broker).
func (c *Controller) SendRaw(id int, payload []byte) error {
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl == nil {
		return errors.New("not connected to the grill controller")
	}
	tok := cl.Publish(fmt.Sprintf("flameboss/%d/recv", id), 1, false, payload)
	if !tok.WaitTimeout(5 * time.Second) {
		return errors.New("the controller didn't answer")
	}
	return tok.Error()
}

// SetTemp changes the pit setpoint (Celsius).
func (c *Controller) SetTemp(celsius float64) error {
	return c.Send(map[string]any{"name": "set_temp", "value": encodeTemp(celsius)})
}

// Silence stops any beeping alarm.
func (c *Controller) Silence() error { return c.Send(map[string]any{"name": "alarm_ack"}) }
