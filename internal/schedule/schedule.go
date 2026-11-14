// Package schedule parses reminder schedules and computes their next
// occurrence. Recurring schedules are evaluated in the device's IANA time
// zone so that daylight-saving changes never shift the wall-clock time.
package schedule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // embed the tz database: works on Windows and in scratch images
)

// Schedule types.
const (
	TypeOnce     = "once"
	TypeDaily    = "daily"
	TypeInterval = "interval"
)

const dateLayout = "2006-01-02"

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Spec is the JSON representation of a schedule, as sent by clients.
//
//	{"type":"once","at":"2026-11-15T08:00:00+01:00"}
//	{"type":"daily","time":"08:00","tz":"Europe/Paris","days":["mon","fri"],"until":"2026-12-31"}
//	{"type":"interval","every":2,"start":"2026-11-15","time":"20:30","tz":"Europe/Paris"}
type Spec struct {
	Type  string   `json:"type"`
	At    string   `json:"at,omitempty"`    // once: RFC 3339 timestamp
	Time  string   `json:"time,omitempty"`  // daily/interval: local "HH:MM"
	TZ    string   `json:"tz,omitempty"`    // daily/interval: IANA zone, e.g. "Europe/Paris"
	Days  []string `json:"days,omitempty"`  // daily: optional weekday filter (mon..sun)
	Every int      `json:"every,omitempty"` // interval: every N days
	Start string   `json:"start,omitempty"` // daily (optional) / interval (required): first local date
	Until string   `json:"until,omitempty"` // daily/interval: optional last local date (inclusive)
}

// Schedule is a validated, ready-to-evaluate schedule.
type Schedule struct {
	spec   Spec
	at     time.Time
	loc    *time.Location
	hour   int
	minute int
	days   [7]bool
	anyDay bool
	start  time.Time // civil date at 00:00 UTC, zero if unset
	until  time.Time // civil date at 00:00 UTC, zero if unset
}

// Parse decodes and validates a JSON schedule.
func Parse(raw []byte) (*Schedule, error) {
	var spec Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}
	return New(spec)
}

// New validates a Spec.
func New(spec Spec) (*Schedule, error) {
	s := &Schedule{spec: spec}
	switch spec.Type {
	case TypeOnce:
		if spec.At == "" {
			return nil, errors.New(`"once" schedule requires "at"`)
		}
		if spec.Time != "" || spec.TZ != "" || len(spec.Days) > 0 || spec.Every != 0 || spec.Start != "" || spec.Until != "" {
			return nil, errors.New(`"once" schedule only accepts "at"`)
		}
		at, err := time.Parse(time.RFC3339, spec.At)
		if err != nil {
			return nil, fmt.Errorf(`invalid "at" (want RFC 3339 with offset): %w`, err)
		}
		s.at = at.UTC().Truncate(time.Second)
		s.spec.At = s.at.Format(time.RFC3339)
		return s, nil

	case TypeDaily, TypeInterval:
		if spec.At != "" {
			return nil, fmt.Errorf(`%q schedule does not accept "at"`, spec.Type)
		}
		if spec.TZ == "" {
			return nil, errors.New(`recurring schedule requires "tz" (IANA zone, e.g. "Europe/Paris")`)
		}
		loc, err := time.LoadLocation(spec.TZ)
		if err != nil || spec.TZ == "Local" {
			return nil, fmt.Errorf("unknown time zone %q", spec.TZ)
		}
		s.loc = loc
		hm, err := time.Parse("15:04", spec.Time)
		if err != nil || len(spec.Time) != 5 {
			return nil, errors.New(`invalid "time" (want "HH:MM", 24h)`)
		}
		s.hour, s.minute = hm.Hour(), hm.Minute()
		if spec.Start != "" {
			if s.start, err = time.Parse(dateLayout, spec.Start); err != nil {
				return nil, errors.New(`invalid "start" (want "YYYY-MM-DD")`)
			}
		}
		if spec.Until != "" {
			if s.until, err = time.Parse(dateLayout, spec.Until); err != nil {
				return nil, errors.New(`invalid "until" (want "YYYY-MM-DD")`)
			}
			if !s.start.IsZero() && s.until.Before(s.start) {
				return nil, errors.New(`"until" is before "start"`)
			}
		}
		if spec.Type == TypeDaily {
			if spec.Every != 0 {
				return nil, errors.New(`"daily" schedule does not accept "every" (use "interval")`)
			}
			if len(spec.Days) == 0 {
				s.anyDay = true
			}
			seen := map[string]bool{}
			norm := make([]string, 0, len(spec.Days))
			for _, d := range spec.Days {
				d = strings.ToLower(d)
				wd, ok := weekdays[d]
				if !ok {
					return nil, fmt.Errorf("invalid day %q (want mon, tue, wed, thu, fri, sat, sun)", d)
				}
				if !seen[d] {
					seen[d] = true
					norm = append(norm, d)
				}
				s.days[wd] = true
			}
			if len(norm) > 0 {
				s.spec.Days = norm
			} else {
				s.spec.Days = nil
			}
		} else {
			if len(spec.Days) > 0 {
				return nil, errors.New(`"interval" schedule does not accept "days"`)
			}
			if spec.Every < 1 || spec.Every > 366 {
				return nil, errors.New(`"interval" schedule requires "every" between 1 and 366`)
			}
			if spec.Start == "" {
				return nil, errors.New(`"interval" schedule requires "start" ("YYYY-MM-DD")`)
			}
			s.anyDay = true
		}
		return s, nil

	case "":
		return nil, errors.New(`schedule requires "type" ("once", "daily" or "interval")`)
	default:
		return nil, fmt.Errorf("unknown schedule type %q", spec.Type)
	}
}

// Spec returns the normalized spec.
func (s *Schedule) Spec() Spec { return s.spec }

// JSON returns the normalized spec as JSON.
func (s *Schedule) JSON() []byte {
	b, _ := json.Marshal(s.spec)
	return b
}

// Next returns the first occurrence strictly after t, in UTC. ok is false
// when the schedule has no more occurrences.
func (s *Schedule) Next(t time.Time) (next time.Time, ok bool) {
	if s.spec.Type == TypeOnce {
		if s.at.After(t) {
			return s.at, true
		}
		return time.Time{}, false
	}

	local := t.In(s.loc)
	day := civil(local.Year(), local.Month(), local.Day())
	if !s.start.IsZero() && day.Before(s.start) {
		day = s.start
	}
	// A daily schedule matches at least one day per week; an interval one
	// matches once every `every` days. One extra day absorbs the case where
	// today's occurrence is already past.
	limit := 8
	if s.spec.Type == TypeInterval {
		limit = s.spec.Every + 1
	}
	for i := 0; i <= limit; i++ {
		d := day.AddDate(0, 0, i)
		if !s.until.IsZero() && d.After(s.until) {
			return time.Time{}, false
		}
		if !s.matches(d) {
			continue
		}
		// time.Date normalizes wall times that don't exist (spring-forward
		// gap) and picks one of the two instants for ambiguous ones
		// (fall-back), always on the requested day.
		cand := time.Date(d.Year(), d.Month(), d.Day(), s.hour, s.minute, 0, 0, s.loc)
		if cand.After(t) {
			return cand.UTC(), true
		}
	}
	return time.Time{}, false
}

func (s *Schedule) matches(d time.Time) bool {
	if s.spec.Type == TypeInterval {
		days := int(d.Sub(s.start).Hours() / 24)
		return days >= 0 && days%s.spec.Every == 0
	}
	return s.anyDay || s.days[d.Weekday()]
}

// civil returns a calendar date as midnight UTC, so that date arithmetic is
// never affected by DST.
func civil(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
