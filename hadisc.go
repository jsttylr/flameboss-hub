package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// HADiscovery publishes the grill to your own MQTT broker (for example the
// Mosquitto add-on in Home Assistant) using Home Assistant's MQTT discovery, so
// the entities appear automatically with no custom integration installed.
type HADiscovery struct {
	mu        sync.Mutex
	client    mqtt.Client
	ctrl      *Controller
	announced int // device ID we've sent discovery configs for
	lastPub   time.Time
	pending   bool
}

func NewHADiscovery(ctrl *Controller) *HADiscovery { return &HADiscovery{ctrl: ctrl} }

func (h *HADiscovery) Start(broker, user, pass string) {
	h.Stop()
	if broker == "" {
		return
	}
	if !strings.Contains(broker, ":") {
		broker += ":1883"
	}
	opts := mqtt.NewClientOptions().
		AddBroker("tcp://" + broker).
		SetClientID(fmt.Sprintf("flameboss-hub-%d", time.Now().UnixNano()%1e8)).
		SetUsername(user).SetPassword(pass).
		SetAutoReconnect(true).SetConnectRetry(true).SetConnectRetryInterval(15 * time.Second).
		SetWill("flameboss_hub/status", "offline", 1, true)
	opts.SetOnConnectHandler(func(cl mqtt.Client) {
		log.Printf("connected to MQTT broker %s for Home Assistant discovery", broker)
		cl.Publish("flameboss_hub/status", 1, true, "online")
		h.mu.Lock()
		h.announced = 0
		h.mu.Unlock()
		cl.Subscribe("flameboss_hub/+/set_temp", 1, h.onCommand)
		cl.Subscribe("flameboss_hub/+/silence", 1, h.onCommand)
		cl.Subscribe("homeassistant/status", 1, func(mqtt.Client, mqtt.Message) {
			// HA restarted: send discovery again.
			h.mu.Lock()
			h.announced = 0
			h.mu.Unlock()
			h.StateChanged(h.ctrl.Snapshot())
		})
		h.StateChanged(h.ctrl.Snapshot())
	})
	cl := mqtt.NewClient(opts)
	h.mu.Lock()
	h.client = cl
	h.mu.Unlock()
	go func() {
		if tok := cl.Connect(); tok.Wait() && tok.Error() != nil {
			log.Printf("can't connect to MQTT broker %s: %v", broker, tok.Error())
		}
	}()
}

func (h *HADiscovery) Stop() {
	h.mu.Lock()
	cl := h.client
	h.client = nil
	h.mu.Unlock()
	if cl != nil {
		cl.Publish("flameboss_hub/status", 1, true, "offline").WaitTimeout(time.Second)
		cl.Disconnect(250)
	}
}

func (h *HADiscovery) onCommand(_ mqtt.Client, m mqtt.Message) {
	var err error
	switch {
	case strings.HasSuffix(m.Topic(), "/set_temp"):
		var c float64
		c, err = strconv.ParseFloat(strings.TrimSpace(string(m.Payload())), 64)
		if err == nil {
			err = h.ctrl.SetTemp(c)
		}
	case strings.HasSuffix(m.Topic(), "/silence"):
		err = h.ctrl.Silence()
	}
	if err != nil {
		log.Printf("Home Assistant command failed: %v", err)
	}
}

func (h *HADiscovery) announce(cl mqtt.Client, s *State) {
	id := s.DeviceID
	base := fmt.Sprintf("flameboss_hub/%d", id)
	dev := map[string]any{
		"identifiers":  []string{fmt.Sprintf("flameboss_%d", id)},
		"name":         fmt.Sprintf("Flame Boss %d", id),
		"manufacturer": "Flame Boss",
		"model":        "Flame Boss / EGG Genius",
		"sw_version":   s.Firmware,
	}
	avail := []map[string]string{
		{"topic": "flameboss_hub/status"},
		{"topic": base + "/connected", "payload_available": "on", "payload_not_available": "off"},
	}
	pub := func(component, key string, cfg map[string]any) {
		cfg["unique_id"] = fmt.Sprintf("flameboss_%d_%s", id, key)
		cfg["object_id"] = fmt.Sprintf("flame_boss_%d_%s", id, key)
		cfg["device"] = dev
		if _, ok := cfg["availability"]; !ok {
			cfg["availability"] = avail
			cfg["availability_mode"] = "all"
		}
		b, _ := json.Marshal(cfg)
		cl.Publish(fmt.Sprintf("homeassistant/%s/flameboss_%d/%s/config", component, id, key), 1, true, b)
	}
	temp := func(key, name, tpl string) {
		pub("sensor", key, map[string]any{
			"name": name, "state_topic": base + "/state", "value_template": tpl,
			"device_class": "temperature", "unit_of_measurement": "°C", "state_class": "measurement",
			"suggested_display_precision": 0,
		})
	}
	temp("pit", "Pit temperature", "{{ value_json.pit }}")
	for i := 1; i <= 3; i++ {
		temp(fmt.Sprintf("meat_%d", i), fmt.Sprintf("Meat probe %d", i), fmt.Sprintf("{{ value_json.meat%d }}", i))
	}
	pub("sensor", "blower", map[string]any{
		"name": "Blower", "state_topic": base + "/state", "value_template": "{{ value_json.blower }}",
		"unit_of_measurement": "%", "state_class": "measurement",
	})
	pub("number", "target", map[string]any{
		"name": "Target pit temperature", "state_topic": base + "/state", "value_template": "{{ value_json.set_temp }}",
		"command_topic": base + "/set_temp", "device_class": "temperature", "unit_of_measurement": "°C",
		"min": s.MinTemp, "max": s.MaxTemp, "step": 0.5, "mode": "box",
	})
	pub("button", "silence", map[string]any{"name": "Silence alarm", "command_topic": base + "/silence"})
	pub("binary_sensor", "alarm", map[string]any{
		"name": "Alarm", "state_topic": base + "/state", "value_template": "{{ 'ON' if value_json.alarm else 'OFF' }}",
		"device_class": "problem",
	})
	pub("binary_sensor", "connected", map[string]any{
		"name": "Connected", "state_topic": base + "/connected", "payload_on": "on", "payload_off": "off",
		"device_class": "connectivity", "entity_category": "diagnostic",
		"availability": []map[string]string{{"topic": "flameboss_hub/status"}},
	})
	pub("sensor", "last_alert", map[string]any{
		"name": "Last alert", "state_topic": base + "/state", "value_template": "{{ value_json.last_alert }}",
	})
}

func jnum(t *float64) any {
	if t == nil {
		return nil
	}
	return *t
}

// StateChanged publishes the latest readings (at most every 2 s).
func (h *HADiscovery) StateChanged(s *State) {
	h.mu.Lock()
	cl := h.client
	if cl == nil || !cl.IsConnectionOpen() || s.DeviceID == 0 {
		h.mu.Unlock()
		return
	}
	if h.announced != s.DeviceID {
		h.announced = s.DeviceID
		h.mu.Unlock()
		h.announce(cl, s)
		h.mu.Lock()
	}
	if time.Since(h.lastPub) < 2*time.Second {
		if !h.pending {
			h.pending = true
			time.AfterFunc(2*time.Second, func() {
				h.mu.Lock()
				h.pending = false
				h.mu.Unlock()
				h.StateChanged(h.ctrl.Snapshot())
			})
		}
		h.mu.Unlock()
		return
	}
	h.lastPub = time.Now()
	h.mu.Unlock()

	base := fmt.Sprintf("flameboss_hub/%d", s.DeviceID)
	conn := map[bool]string{true: "on", false: "off"}[s.Connected]
	cl.Publish(base+"/connected", 1, true, conn)
	b, _ := json.Marshal(map[string]any{
		"pit": jnum(s.Temps[0]), "meat1": jnum(s.Temps[1]), "meat2": jnum(s.Temps[2]), "meat3": jnum(s.Temps[3]),
		"set_temp": jnum(s.SetTemp), "blower": jnum(s.Blower), "alarm": s.Alarm(), "last_alert": s.LastAlert,
	})
	cl.Publish(base+"/state", 0, true, b)
}

func (h *HADiscovery) Alert(string, *State)  {}
func (h *HADiscovery) Raw(int, RawMessage) {}
