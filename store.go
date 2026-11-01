package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps cook history in a SQLite file in the data folder.
type Store struct{ db *sql.DB }

// Cook is one cooking session.
type Cook struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Notes    string     `json:"notes"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	Labels   [4]string  `json:"labels"`
	MaxPit   *float64   `json:"max_pit,omitempty"`
	Samples  int        `json:"samples"`
}

// Sample is one reading during a cook (Celsius; nil = no probe).
type Sample struct {
	T       time.Time `json:"t"`
	Temps   [4]Temp   `json:"temps"`
	SetTemp Temp      `json:"set_temp"`
	Blower  *float64  `json:"blower"`
}

// Event is something notable during a cook (setpoint change, alert, note).
type Event struct {
	T    time.Time `json:"t"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "cooks.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS cooks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  started INTEGER NOT NULL,
  ended INTEGER,
  labels TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS samples (
  cook_id INTEGER NOT NULL REFERENCES cooks(id) ON DELETE CASCADE,
  t INTEGER NOT NULL,
  pit REAL, m1 REAL, m2 REAL, m3 REAL, set_temp REAL, blower REAL
);
CREATE INDEX IF NOT EXISTS samples_cook ON samples(cook_id, t);
CREATE TABLE IF NOT EXISTS events (
  cook_id INTEGER NOT NULL REFERENCES cooks(id) ON DELETE CASCADE,
  t INTEGER NOT NULL,
  kind TEXT NOT NULL,
  text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_cook ON events(cook_id, t);
`)
	if err != nil {
		return nil, fmt.Errorf("setting up the cook database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func joinLabels(l [4]string) string {
	return l[0] + "\x1f" + l[1] + "\x1f" + l[2] + "\x1f" + l[3]
}

func splitLabels(s string) (out [4]string) {
	i := 0
	start := 0
	for j := 0; j <= len(s) && i < 4; j++ {
		if j == len(s) || s[j] == 0x1f {
			out[i] = s[start:j]
			i++
			start = j + 1
		}
	}
	return
}

func (s *Store) StartCook(name string, at time.Time, labels [4]string) (int64, error) {
	r, err := s.db.Exec(`INSERT INTO cooks(name, started, labels) VALUES(?,?,?)`, name, at.UnixMilli(), joinLabels(labels))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) EndCook(id int64, at time.Time) error {
	_, err := s.db.Exec(`UPDATE cooks SET ended=? WHERE id=? AND ended IS NULL`, at.UnixMilli(), id)
	return err
}

func (s *Store) UpdateCook(id int64, name, notes string) error {
	_, err := s.db.Exec(`UPDATE cooks SET name=?, notes=? WHERE id=?`, name, notes, id)
	return err
}

func (s *Store) DeleteCook(id int64) error {
	_, err := s.db.Exec(`DELETE FROM cooks WHERE id=?`, id)
	return err
}

// OpenCook returns the cook that hasn't ended, if any.
func (s *Store) OpenCook() (*Cook, error) {
	cooks, err := s.listCooks(`WHERE c.ended IS NULL ORDER BY c.started DESC LIMIT 1`)
	if err != nil || len(cooks) == 0 {
		return nil, err
	}
	return &cooks[0], nil
}

func (s *Store) Cooks(limit int) ([]Cook, error) {
	return s.listCooks(fmt.Sprintf(`ORDER BY c.started DESC LIMIT %d`, limit))
}

func (s *Store) Cook(id int64) (*Cook, error) {
	cooks, err := s.listCooks(fmt.Sprintf(`WHERE c.id=%d`, id))
	if err != nil || len(cooks) == 0 {
		return nil, err
	}
	return &cooks[0], nil
}

func (s *Store) listCooks(tail string) ([]Cook, error) {
	rows, err := s.db.Query(`SELECT c.id, c.name, c.notes, c.started, c.ended, c.labels,
  (SELECT MAX(pit) FROM samples WHERE cook_id=c.id), (SELECT COUNT(*) FROM samples WHERE cook_id=c.id)
FROM cooks c ` + tail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cook
	for rows.Next() {
		var c Cook
		var started int64
		var ended sql.NullInt64
		var labels string
		var maxPit sql.NullFloat64
		if err := rows.Scan(&c.ID, &c.Name, &c.Notes, &started, &ended, &labels, &maxPit, &c.Samples); err != nil {
			return nil, err
		}
		c.Started = time.UnixMilli(started)
		if ended.Valid {
			t := time.UnixMilli(ended.Int64)
			c.Ended = &t
		}
		if maxPit.Valid {
			c.MaxPit = &maxPit.Float64
		}
		c.Labels = splitLabels(labels)
		out = append(out, c)
	}
	return out, rows.Err()
}

func nf(t Temp) any {
	if t == nil {
		return nil
	}
	return *t
}

func (s *Store) AddSample(cookID int64, smp Sample) error {
	_, err := s.db.Exec(`INSERT INTO samples(cook_id,t,pit,m1,m2,m3,set_temp,blower) VALUES(?,?,?,?,?,?,?,?)`,
		cookID, smp.T.UnixMilli(), nf(smp.Temps[0]), nf(smp.Temps[1]), nf(smp.Temps[2]), nf(smp.Temps[3]), nf(smp.SetTemp), nf(smp.Blower))
	return err
}

func (s *Store) AddEvent(cookID int64, e Event) error {
	_, err := s.db.Exec(`INSERT INTO events(cook_id,t,kind,text) VALUES(?,?,?,?)`, cookID, e.T.UnixMilli(), e.Kind, e.Text)
	return err
}

func ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func (s *Store) Samples(cookID int64) ([]Sample, error) {
	rows, err := s.db.Query(`SELECT t,pit,m1,m2,m3,set_temp,blower FROM samples WHERE cook_id=? ORDER BY t`, cookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var t int64
		var v [6]sql.NullFloat64
		if err := rows.Scan(&t, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
			return nil, err
		}
		out = append(out, Sample{T: time.UnixMilli(t), Temps: [4]Temp{ptr(v[0]), ptr(v[1]), ptr(v[2]), ptr(v[3])}, SetTemp: ptr(v[4]), Blower: ptr(v[5])})
	}
	return out, rows.Err()
}

func (s *Store) Events(cookID int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT t,kind,text FROM events WHERE cook_id=? ORDER BY t`, cookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var t int64
		var e Event
		if err := rows.Scan(&t, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.T = time.UnixMilli(t)
		out = append(out, e)
	}
	return out, rows.Err()
}
