package stats

import (
	"math"
	"testing"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

func readings(start time.Time, vals ...float64) []store.Reading {
	rs := make([]store.Reading, len(vals))
	for i, v := range vals {
		rs[i] = store.Reading{Time: start.Add(time.Duration(i) * 5 * time.Minute).UnixMilli(), Value: v}
	}
	return rs
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestSummarize(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rs := readings(start, 50, 60, 100, 140, 150, 200, 300, 100, 100, 100)
	s := Summarize(rs, start, start.Add(50*time.Minute))
	if s.Count != 10 || !near(s.Mean, 130) || !near(s.Coverage, 100) {
		t.Fatalf("count/mean/coverage: %+v", s)
	}
	r := s.Ranges
	if !near(r.VeryLow, 10) || !near(r.Low, 10) || !near(r.InRange, 60) || !near(r.High, 10) || !near(r.VeryHigh, 10) || !near(r.Tight, 50) {
		t.Fatalf("ranges: %+v", r)
	}
	if !near(s.GMI, 3.31+0.02392*130) {
		t.Fatalf("gmi: %v", s.GMI)
	}
	if s.Min != 50 || s.Max != 300 || s.SD <= 0 || !near(s.CV, s.SD/s.Mean*100) {
		t.Fatalf("spread: %+v", s)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil, time.Now().Add(-time.Hour), time.Now())
	if s.Count != 0 || s.Mean != 0 || s.Coverage != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestLowEvents(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 10 min low (too short), then 20 min low (counts), then low at the end for 15 min (counts).
	rs := readings(start, 100, 65, 65, 100, 100, 60, 60, 60, 60, 100, 100, 68, 68, 68, 68)
	if n := lowEvents(rs); n != 2 {
		t.Fatalf("got %d events, want 2", n)
	}
}

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	if Percentile(v, 50) != 3 || Percentile(v, 0) != 1 || Percentile(v, 100) != 5 || !near(Percentile(v, 25), 2) {
		t.Fatal("percentile")
	}
	if !near(Percentile([]float64{10, 20}, 50), 15) {
		t.Fatal("interpolation")
	}
}

func TestAGPAndDailyUseLocalTime(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	// 07:00 local on two days, high readings; everything else unrecorded.
	d1 := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	d2 := d1.AddDate(0, 0, 1)
	rs := append(readings(d1, 200, 210, 220), readings(d2, 200, 210, 220)...)
	agp := AGP(rs, loc, 15)
	if len(agp) != 96 {
		t.Fatalf("buckets: %d", len(agp))
	}
	b := agp[7*4] // 07:00
	if b.N != 6 || b.P50 != 210 || b.P5 < 200 || b.P95 > 220 {
		t.Fatalf("07:00 bucket: %+v", b)
	}
	if agp[0].N != 0 {
		t.Fatalf("midnight bucket should be empty: %+v", agp[0])
	}
	days := Daily(rs, loc)
	if len(days) != 2 || days[0].Date != "2026-01-01" || !near(days[0].High, 100) {
		t.Fatalf("daily: %+v", days)
	}
}
