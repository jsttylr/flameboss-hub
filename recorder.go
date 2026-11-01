package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Recorder turns the live readings into cook history.
//
// A cook starts by itself when the pit probe reads at least 50 C (the fire is
// lit), or when you press Start. It ends by itself after the pit has been below
// 40 C, unplugged or offline for 30 minutes, or when you press Stop.
type Recorder struct {
	mu        sync.Mutex
	store     *Store
	cookID    int64
	lastSave  time.Time
	coldSince time.Time
	lastSet   Temp
	now       func() time.Time
	Interval  time.Duration
	OnChange  func() // called when a cook starts or ends
}

const (
	autoStartC = 50.0
	autoEndC   = 40.0
	autoEndFor = 30 * time.Minute
)

func NewRecorder(store *Store) (*Recorder, error) {
	r := &Recorder{store: store, now: time.Now, Interval: 15 * time.Second}
	open, err := store.OpenCook()
	if err != nil {
		return nil, err
	}
	if open != nil {
		r.cookID = open.ID
	}
	return r, nil
}

// Current returns the ID of the cook in progress (0 = none).
func (r *Recorder) Current() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cookID
}

func (r *Recorder) Start(name string, labels [4]string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked(name, labels)
}

func (r *Recorder) startLocked(name string, labels [4]string) (int64, error) {
	if r.cookID != 0 {
		return r.cookID, nil
	}
	now := r.now()
	if name == "" {
		name = "Cook on " + now.Format("Mon Jan 2")
	}
	id, err := r.store.StartCook(name, now, labels)
	if err != nil {
		return 0, err
	}
	r.cookID, r.lastSave, r.coldSince, r.lastSet = id, time.Time{}, time.Time{}, nil
	log.Printf("cook %d started: %s", id, name)
	if r.OnChange != nil {
		go r.OnChange()
	}
	return id, nil
}

func (r *Recorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopLocked()
}

func (r *Recorder) stopLocked() error {
	if r.cookID == 0 {
		return nil
	}
	err := r.store.EndCook(r.cookID, r.now())
	log.Printf("cook %d ended", r.cookID)
	r.cookID = 0
	if r.OnChange != nil {
		go r.OnChange()
	}
	return err
}

// StateChanged is called for every update from the controller.
func (r *Recorder) StateChanged(s *State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	pit := s.Temps[0]
	hot := s.Connected && pit != nil && *pit >= autoStartC
	cold := !s.Connected || pit == nil || *pit < autoEndC

	if r.cookID == 0 {
		if hot {
			if _, err := r.startLocked("", s.Labels); err != nil {
				log.Printf("couldn't start a cook: %v", err)
			}
		}
		if r.cookID == 0 {
			return
		}
	}
	if cold {
		if r.coldSince.IsZero() {
			r.coldSince = now
		} else if now.Sub(r.coldSince) >= autoEndFor {
			if err := r.stopLocked(); err != nil {
				log.Printf("couldn't end the cook: %v", err)
			}
			return
		}
	} else {
		r.coldSince = time.Time{}
	}
	if !s.Connected {
		return
	}
	setChanged := s.SetTemp != nil && (r.lastSet == nil || *r.lastSet != *s.SetTemp)
	if setChanged {
		if r.lastSet != nil {
			r.store.AddEvent(r.cookID, Event{T: now, Kind: "set_temp", Text: fmt.Sprintf("%.1f", *s.SetTemp)})
		}
		v := *s.SetTemp
		r.lastSet = &v
	}
	if setChanged || now.Sub(r.lastSave) >= r.Interval {
		if err := r.store.AddSample(r.cookID, Sample{T: now, Temps: s.Temps, SetTemp: s.SetTemp, Blower: s.Blower}); err != nil {
			log.Printf("couldn't save a reading: %v", err)
		}
		r.lastSave = now
	}
}

func (r *Recorder) Alert(text string, s *State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cookID != 0 {
		r.store.AddEvent(r.cookID, Event{T: r.now(), Kind: "alert", Text: text})
	}
}

func (r *Recorder) Raw(int, RawMessage) {}

// AddNote adds a timestamped note to the cook in progress.
func (r *Recorder) AddNote(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cookID == 0 {
		return fmt.Errorf("no cook in progress")
	}
	return r.store.AddEvent(r.cookID, Event{T: r.now(), Kind: "note", Text: text})
}
