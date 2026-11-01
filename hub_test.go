package main

import (
	"encoding/json"
	"testing"
	"time"
)

func msg(t *testing.T, s string) map[string]any {
	t.Helper()
	m := decodeMessage([]byte(s))
	if m == nil {
		t.Fatalf("couldn't decode %s", s)
	}
	return m
}

// Messages captured from a real EGG Genius (firmware 4.27.28) on 2026-10-31.
func TestApplyRealDump(t *testing.T) {
	s := newState()
	now := time.Now()
	for _, line := range []string{
		`{"name":"id","hw_id":10,"device_id":123456,"uid":"6Gjn6CUdAAAAAAAAAAAAAA==","pin":1234,"sn":82922,"disabled":false}`,
		`{"name":"cook","id":0,"private":false,"next_sec":1793488624}`,
		`{"name":"set_temp_limits","min":378,"max":2322}`,
		`{"name":"set_temp","value":1767}`,
		`{"name":"wifi_cx","connected":true,"rssi":-54}`,
		`{"name":"meat_alarm","sensor":1,"action":"off","done_temp":906,"warm_temp":822}`,
		`{"name":"dc_input","value":123}`,
		`{"name":"versions","hw_id":10,"app":"4.27.28","pos":0,"next_addr":528384,"revert_avail":null}`,
		`{"name":"temps","cook_id":0,"sec":1793488624,"temps":[1072,655,-32767,-32767],"set_temp":1767,"blower":2550}`,
	} {
		s.Apply(msg(t, line), now)
	}
	if s.DeviceID != 123456 || s.Firmware != "4.27.28" {
		t.Errorf("id/firmware: %d %q", s.DeviceID, s.Firmware)
	}
	if *s.SetTemp != 176.7 || s.MinTemp != 37.8 || s.MaxTemp != 232.2 {
		t.Errorf("setpoint/limits: %v %v %v", *s.SetTemp, s.MinTemp, s.MaxTemp)
	}
	if *s.Temps[0] != 107.2 || *s.Temps[1] != 65.5 || s.Temps[2] != nil {
		t.Errorf("temps: %v", s.Temps)
	}
	if *s.Blower != 25.5 || *s.WifiRSSI != -54 || *s.Voltage != 12.3 {
		t.Errorf("blower/wifi/voltage: %v %v %v", *s.Blower, *s.WifiRSSI, *s.Voltage)
	}
	if a := s.MeatAlarms[1]; a.Action != "off" || *a.DoneTemp != 90.6 {
		t.Errorf("meat alarm: %+v", a)
	}
	b, _ := json.Marshal(s)
	if json.Valid(b) == false {
		t.Fatal("state doesn't marshal")
	}
	if got := s.Apply(msg(t, `{"name":"meat_alarm_triggered","sensor":1}`), now); got != "Meat 1 is done" {
		t.Errorf("alert text: %q", got)
	}
}

func TestHexAndRedact(t *testing.T) {
	m := decodeMessage([]byte("7b226e616d65223a2273796e63227d"))
	if m["name"] != "sync" {
		t.Fatalf("hex decode: %v", m)
	}
	r := redact(map[string]any{"name": "wifi", "key": "secret", "pin": 1234})
	if r["key"] != "****" || r["pin"] != "****" {
		t.Fatalf("redact: %v", r)
	}
}

func f(v float64) *float64 { return &v }

func TestRecorderAutoStartStop(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec, _ := NewRecorder(store)
	clock := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return clock }

	s := newState()
	s.Connected = true
	s.Temps[0] = f(30) // cold grill: no cook
	rec.StateChanged(s)
	if rec.Current() != 0 {
		t.Fatal("started a cook on a cold grill")
	}
	s.Temps[0], s.SetTemp = f(60), f(107)
	rec.StateChanged(s)
	id := rec.Current()
	if id == 0 {
		t.Fatal("didn't start a cook when the pit got hot")
	}
	for i := 0; i < 8; i++ { // 2 minutes of readings, saved every 15 s
		clock = clock.Add(15 * time.Second)
		rec.StateChanged(s)
	}
	clock = clock.Add(time.Second)
	s.SetTemp = f(120)
	rec.StateChanged(s)
	rec.AddNote("wrapped")
	samples, _ := store.Samples(id)
	events, _ := store.Events(id)
	if len(samples) != 10 {
		t.Errorf("samples: got %d, want 10", len(samples))
	}
	if len(events) != 2 || events[0].Kind != "set_temp" || events[1].Text != "wrapped" {
		t.Errorf("events: %+v", events)
	}
	// Fire goes out: ends after 30 minutes cold.
	s.Temps[0] = f(35)
	rec.StateChanged(s)
	clock = clock.Add(29 * time.Minute)
	rec.StateChanged(s)
	if rec.Current() == 0 {
		t.Fatal("ended too early")
	}
	clock = clock.Add(2 * time.Minute)
	rec.StateChanged(s)
	if rec.Current() != 0 {
		t.Fatal("didn't end after 30 minutes cold")
	}
	c, _ := store.Cook(id)
	if c.Ended == nil || c.MaxPit == nil || *c.MaxPit != 60 {
		t.Errorf("cook: %+v", c)
	}
	// Restart picks up an open cook.
	rec.Start("Brisket", [4]string{})
	rec2, _ := NewRecorder(store)
	if rec2.Current() == 0 {
		t.Error("open cook not resumed after restart")
	}
}
