package main

// The Flame Boss / EGG Genius local protocol, as documented by Flame Boss
// (https://github.com/flameboss/fb-api-doc) and confirmed on real hardware:
//
//   - MQTT 3.1.1 to the controller on port 1883, username "fb", password = device PIN.
//   - Subscribe to flameboss/<id>/send/# at QoS 1 and acknowledge every message,
//     and ping every 5 s, or the controller stops talking to you after ~10 s.
//   - Each new subscriber gets a full dump of settings and readings.
//   - Send JSON commands ("downlinks") to flameboss/<id>/recv.
//
// Temperatures are tenths of a degree Celsius; -32767 means no probe.

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"
)

const noProbe = -32767

// Temp is a temperature in Celsius, or nil when the probe is unplugged.
type Temp = *float64

func decodeTemp(v any) Temp {
	f, ok := num(v)
	if !ok || f == noProbe {
		return nil
	}
	c := math.Round(f) / 10
	return &c
}

func encodeTemp(c float64) int { return int(math.Round(c * 10)) }

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// decodeMessage parses one controller message (plain JSON, or hex-encoded JSON on old firmware).
func decodeMessage(payload []byte) map[string]any {
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "{") {
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil
		}
		s = string(b)
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}

var secretKeys = map[string]bool{"pin": true, "key": true, "password": true, "pass": true, "psk": true, "k": true}

// redact hides the PIN and Wi-Fi passwords before anything is logged or shown.
func redact(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if secretKeys[k] {
			out[k] = "****"
		} else {
			out[k] = v
		}
	}
	return out
}

// MeatAlarm is the controller's per-probe "meat done" setting.
type MeatAlarm struct {
	Action   string `json:"action"` // off, on, keep_warm
	DoneTemp Temp   `json:"done_temp"`
	WarmTemp Temp   `json:"warm_temp"`
}

// State is everything we know about one controller.
type State struct {
	Connected   bool               `json:"connected"`
	DeviceID    int                `json:"device_id,omitempty"`
	HwID        int                `json:"hw_id,omitempty"`
	Firmware    string             `json:"firmware,omitempty"`
	Temps       [4]Temp            `json:"temps"` // pit, meat 1-3
	SetTemp     Temp               `json:"set_temp"`
	Blower      *float64           `json:"blower"` // percent
	CookID      int                `json:"cook_id"`
	MinTemp     float64            `json:"min_temp"`
	MaxTemp     float64            `json:"max_temp"`
	Labels      [4]string          `json:"labels"`
	MeatAlarms  map[int]*MeatAlarm `json:"meat_alarms"`
	PitAlarm    map[string]any     `json:"pit_alarm,omitempty"`
	Timer       map[string]any     `json:"timer,omitempty"`
	SoundAlarm  bool               `json:"sound_alarm"`
	WifiRSSI    *int               `json:"wifi_rssi,omitempty"`
	Voltage     *float64           `json:"voltage,omitempty"`
	LastAlert   string             `json:"last_alert,omitempty"`
	LastAlertAt *time.Time         `json:"last_alert_at,omitempty"`
	LastMessage *time.Time         `json:"last_message,omitempty"`
}

func newState() *State {
	return &State{MinTemp: 37.8, MaxTemp: 232.2, MeatAlarms: map[int]*MeatAlarm{}}
}

// Alarm reports whether the controller is beeping about something.
func (s *State) Alarm() bool {
	return s.SoundAlarm || s.PitAlarm["status"] == "alerting"
}

// alertNames are one-off messages worth telling the user about.
var alertNames = map[string]string{
	"meat_alarm_triggered": "Meat is done",
	"pit_alarm_triggered":  "Pit temperature out of range",
	"pit_alarm_active":     "Pit alarm armed",
	"vent_advice":          "Pit has run hot for a while: close the vents a bit",
	"probe_overtemp":       "A probe is too hot",
	"device_overtemp":      "The controller is too hot",
}

// Apply updates the state from one message. It returns a human alert text for
// one-off alert messages, else "".
func (s *State) Apply(m map[string]any, now time.Time) string {
	s.LastMessage = &now
	name, _ := m["name"].(string)
	switch name {
	case "temps":
		if arr, ok := m["temps"].([]any); ok {
			for i := range s.Temps {
				if i < len(arr) {
					s.Temps[i] = decodeTemp(arr[i])
				} else {
					s.Temps[i] = nil
				}
			}
		}
		if v, ok := m["set_temp"]; ok {
			s.SetTemp = decodeTemp(v)
		}
		if f, ok := num(m["blower"]); ok {
			b := math.Round(f) / 100
			s.Blower = &b
		}
		if f, ok := num(m["cook_id"]); ok {
			s.CookID = int(f)
		}
	case "cook":
		if f, ok := num(m["id"]); ok {
			s.CookID = int(f)
		}
	case "set_temp":
		if v, ok := m["value"]; ok {
			s.SetTemp = decodeTemp(v)
		}
	case "id":
		if f, ok := num(m["device_id"]); ok {
			s.DeviceID = int(f)
		}
		if f, ok := num(m["hw_id"]); ok {
			s.HwID = int(f)
		}
	case "versions":
		if v, ok := m["app"].(string); ok {
			s.Firmware = v
		}
	case "set_temp_limits":
		if t := decodeTemp(m["min"]); t != nil {
			s.MinTemp = *t
		}
		if t := decodeTemp(m["max"]); t != nil {
			s.MaxTemp = *t
		}
	case "labels":
		if arr, ok := m["values"].([]any); ok {
			for i := range s.Labels {
				s.Labels[i] = ""
				if i < len(arr) {
					s.Labels[i], _ = arr[i].(string)
				}
			}
		}
	case "meat_alarm":
		if f, ok := num(m["sensor"]); ok {
			a := &MeatAlarm{DoneTemp: decodeTemp(m["done_temp"]), WarmTemp: decodeTemp(m["warm_temp"])}
			switch v := m["action"].(type) {
			case string:
				a.Action = v
			case bool:
				a.Action = map[bool]string{true: "on", false: "off"}[v]
			}
			s.MeatAlarms[int(f)] = a
		}
	case "pit_alarm":
		s.PitAlarm = without(m, "name")
	case "timer":
		s.Timer = without(m, "name")
	case "sound":
		s.SoundAlarm = m["status"] == "alarm"
	case "wifi_cx":
		if f, ok := num(m["rssi"]); ok {
			r := int(f)
			s.WifiRSSI = &r
		}
	case "dc_input":
		if f, ok := num(m["value"]); ok {
			v := f / 10
			s.Voltage = &v
		}
	default:
		if text, ok := alertNames[name]; ok {
			s.LastAlert = text
			if name == "meat_alarm_triggered" {
				if f, ok := num(m["sensor"]); ok && int(f) >= 1 && int(f) <= 3 {
					label := s.Labels[int(f)]
					if label == "" {
						label = "Meat " + string(rune('0'+int(f)))
					}
					s.LastAlert = label + " is done"
				}
			}
			s.LastAlertAt = &now
			return s.LastAlert
		}
	}
	return ""
}

func without(m map[string]any, key string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// Clone returns a deep-enough copy for handing to other goroutines.
func (s *State) Clone() *State {
	c := *s
	c.MeatAlarms = make(map[int]*MeatAlarm, len(s.MeatAlarms))
	for k, v := range s.MeatAlarms {
		a := *v
		c.MeatAlarms[k] = &a
	}
	c.PitAlarm = without(s.PitAlarm, "")
	c.Timer = without(s.Timer, "")
	return &c
}
