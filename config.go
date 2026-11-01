package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Config holds the settings you change on the Settings page. It's saved as
// config.json in the data folder. Environment variables fill in the first-run values.
type Config struct {
	Host  string `json:"host"`  // grill controller IP address
	Port  int    `json:"port"`  // usually 1883
	PIN   string `json:"pin"`   // controller PIN
	Units string `json:"units"` // "F" or "C"

	// Built-in broker: lets Home Assistant (or anything that speaks the Flame
	// Boss protocol) connect to this app instead of the grill.
	MirrorEnabled bool   `json:"mirror_enabled"`
	MirrorPort    int    `json:"mirror_port"`
	MirrorPIN     string `json:"mirror_pin"` // blank = same as the controller PIN

	// Optional: publish to your own MQTT broker with Home Assistant discovery.
	HAEnabled  bool   `json:"ha_enabled"`
	HABroker   string `json:"ha_broker"` // host or host:port
	HAUser     string `json:"ha_user"`
	HAPassword string `json:"ha_password"`

	// Optional: push alerts to your phone through ntfy (https://ntfy.sh or self-hosted).
	NtfyURL string `json:"ntfy_url"` // e.g. https://ntfy.sh/my-grill-topic
}

type ConfigStore struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func LoadConfig(dir string) (*ConfigStore, error) {
	cs := &ConfigStore{path: filepath.Join(dir, "config.json")}
	cs.cfg = Config{
		Host:          os.Getenv("FLAMEBOSS_HOST"),
		Port:          envInt("FLAMEBOSS_PORT", 1883),
		PIN:           os.Getenv("FLAMEBOSS_PIN"),
		Units:         env("FLAMEBOSS_UNITS", "F"),
		MirrorEnabled: os.Getenv("FLAMEBOSS_MIRROR") != "0",
		MirrorPort:    envInt("FLAMEBOSS_MIRROR_PORT", 1883),
		NtfyURL:       os.Getenv("FLAMEBOSS_NTFY_URL"),
	}
	b, err := os.ReadFile(cs.path)
	if err == nil {
		if err := json.Unmarshal(b, &cs.cfg); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return cs, nil
}

func (cs *ConfigStore) Get() Config {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.cfg
}

func (cs *ConfigStore) Set(c Config) error {
	if c.Port == 0 {
		c.Port = 1883
	}
	if c.MirrorPort == 0 {
		c.MirrorPort = 1883
	}
	if c.Units != "C" {
		c.Units = "F"
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cs.path); err != nil {
		return err
	}
	cs.cfg = c
	return nil
}

// Public is the config with secrets blanked, for the Settings page.
func (c Config) Public() map[string]any {
	return map[string]any{
		"host": c.Host, "port": c.Port, "pin_set": c.PIN != "", "units": c.Units,
		"mirror_enabled": c.MirrorEnabled, "mirror_port": c.MirrorPort, "mirror_pin_set": c.MirrorPIN != "",
		"ha_enabled": c.HAEnabled, "ha_broker": c.HABroker, "ha_user": c.HAUser, "ha_password_set": c.HAPassword != "",
		"ntfy_url": c.NtfyURL,
	}
}
