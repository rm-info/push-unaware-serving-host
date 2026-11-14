package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, js string) *Schedule {
	t.Helper()
	s, err := Parse([]byte(js))
	if err != nil {
		t.Fatalf("Parse(%s): %v", js, err)
	}
	return s
}

func ts(t *testing.T, v string) time.Time {
	t.Helper()
	x, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func expectNext(t *testing.T, s *Schedule, after, want string) {
	t.Helper()
	got, ok := s.Next(ts(t, after))
	if want == "" {
		if ok {
			t.Fatalf("Next(%s) = %s, want none", after, got.Format(time.RFC3339))
		}
		return
	}
	if !ok {
		t.Fatalf("Next(%s) = none, want %s", after, want)
	}
	if !got.Equal(ts(t, want)) {
		t.Fatalf("Next(%s) = %s, want %s", after, got.Format(time.RFC3339), want)
	}
}

func TestOnce(t *testing.T) {
	s := mustParse(t, `{"type":"once","at":"2026-11-15T08:00:00+01:00"}`)
	expectNext(t, s, "2026-11-15T06:59:59Z", "2026-11-15T07:00:00Z")
	expectNext(t, s, "2026-11-15T07:00:00Z", "")
	if s.Spec().At != "2026-11-15T07:00:00Z" {
		t.Fatalf("normalized at = %s", s.Spec().At)
	}
}

func TestDaily(t *testing.T) {
	s := mustParse(t, `{"type":"daily","time":"08:00","tz":"Europe/Paris"}`)
	expectNext(t, s, "2026-11-14T06:00:00Z", "2026-11-14T07:00:00Z")
	expectNext(t, s, "2026-11-14T07:00:00Z", "2026-11-15T07:00:00Z") // strictly after
	expectNext(t, s, "2026-11-14T23:30:00Z", "2026-11-15T07:00:00Z") // already the 15th in Paris
}

func TestDailyAcrossDST(t *testing.T) {
	s := mustParse(t, `{"type":"daily","time":"08:00","tz":"Europe/Paris"}`)
	// Spring forward (2026-03-29): 08:00 CET = 07:00Z, then 08:00 CEST = 06:00Z.
	expectNext(t, s, "2026-03-28T07:30:00Z", "2026-03-29T06:00:00Z")
	// Fall back (2026-10-25): 08:00 CEST = 06:00Z, then 08:00 CET = 07:00Z.
	expectNext(t, s, "2026-10-24T06:30:00Z", "2026-10-25T07:00:00Z")

	ny := mustParse(t, `{"type":"daily","time":"09:15","tz":"America/New_York"}`)
	expectNext(t, ny, "2026-03-07T15:00:00Z", "2026-03-08T13:15:00Z") // EDT starts 2026-03-08
}

func TestNonexistentLocalTime(t *testing.T) {
	// 02:30 does not exist in Paris on 2026-03-29. It must still fire that
	// day, exactly once, and the next day at 02:30 CEST.
	s := mustParse(t, `{"type":"daily","time":"02:30","tz":"Europe/Paris"}`)
	got, ok := s.Next(ts(t, "2026-03-28T12:00:00Z"))
	if !ok {
		t.Fatal("no occurrence")
	}
	paris, _ := time.LoadLocation("Europe/Paris")
	if d := got.In(paris); d.Day() != 29 {
		t.Fatalf("occurrence on wrong day: %s", d)
	}
	expectNext(t, s, got.Format(time.RFC3339), "2026-03-30T00:30:00Z")
}

func TestAmbiguousLocalTimeFiresOnce(t *testing.T) {
	// 02:30 happens twice in Paris on 2026-10-25: fire once only.
	s := mustParse(t, `{"type":"daily","time":"02:30","tz":"Europe/Paris"}`)
	first, ok := s.Next(ts(t, "2026-10-24T12:00:00Z"))
	if !ok {
		t.Fatal("no occurrence")
	}
	second, _ := s.Next(first)
	if second.Sub(first) < 23*time.Hour {
		t.Fatalf("fired twice: %s then %s", first, second)
	}
}

func TestDailyWeekdays(t *testing.T) {
	// 2026-11-14 is a Saturday.
	s := mustParse(t, `{"type":"daily","time":"20:00","tz":"UTC","days":["MON","fri","mon"]}`)
	expectNext(t, s, "2026-11-14T00:00:00Z", "2026-11-16T20:00:00Z")
	expectNext(t, s, "2026-11-16T20:00:00Z", "2026-11-20T20:00:00Z")
	if got := s.Spec().Days; len(got) != 2 || got[0] != "mon" || got[1] != "fri" {
		t.Fatalf("days not normalized: %v", got)
	}
}

func TestStartUntil(t *testing.T) {
	s := mustParse(t, `{"type":"daily","time":"08:00","tz":"UTC","start":"2026-12-01","until":"2026-12-02"}`)
	expectNext(t, s, "2026-11-14T00:00:00Z", "2026-12-01T08:00:00Z")
	expectNext(t, s, "2026-12-01T08:00:00Z", "2026-12-02T08:00:00Z")
	expectNext(t, s, "2026-12-02T08:00:00Z", "")
}

func TestInterval(t *testing.T) {
	s := mustParse(t, `{"type":"interval","every":3,"start":"2026-11-10","time":"21:00","tz":"Europe/Paris"}`)
	// Occurrences: 10, 13, 16, ... November.
	expectNext(t, s, "2026-11-01T00:00:00Z", "2026-11-10T20:00:00Z")
	expectNext(t, s, "2026-11-14T00:00:00Z", "2026-11-16T20:00:00Z")
	expectNext(t, s, "2026-11-16T20:00:00Z", "2026-11-19T20:00:00Z")
	// Across the March DST change (2027-03-28), still 21:00 local.
	expectNext(t, s, "2027-03-24T00:00:00Z", "2027-03-25T20:00:00Z")
	expectNext(t, s, "2027-03-25T21:00:00Z", "2027-03-28T19:00:00Z") // DST day itself: 21:00 CEST
}

func TestInvalid(t *testing.T) {
	bad := []string{
		`{}`,
		`{"type":"weekly"}`,
		`{"type":"once"}`,
		`{"type":"once","at":"2026-11-15 08:00"}`,
		`{"type":"once","at":"2026-11-15T08:00:00Z","tz":"UTC"}`,
		`{"type":"daily","time":"08:00"}`,
		`{"type":"daily","time":"8:00","tz":"UTC"}`,
		`{"type":"daily","time":"24:00","tz":"UTC"}`,
		`{"type":"daily","time":"08:00","tz":"Mars/Olympus"}`,
		`{"type":"daily","time":"08:00","tz":"Local"}`,
		`{"type":"daily","time":"08:00","tz":"UTC","days":["lun"]}`,
		`{"type":"daily","time":"08:00","tz":"UTC","every":2}`,
		`{"type":"daily","time":"08:00","tz":"UTC","start":"2026-12-02","until":"2026-12-01"}`,
		`{"type":"interval","time":"08:00","tz":"UTC","start":"2026-12-01"}`,
		`{"type":"interval","every":2,"time":"08:00","tz":"UTC"}`,
		`{"type":"daily","time":"08:00","tz":"UTC","extra":1}`,
	}
	for _, js := range bad {
		if _, err := Parse([]byte(js)); err == nil {
			t.Errorf("Parse(%s) succeeded, want error", js)
		}
	}
}
