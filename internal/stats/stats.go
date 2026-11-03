// Package stats computes glycemic metrics following the international
// consensus on CGM reporting (Battelino et al., Diabetes Care 2019) and the
// Ambulatory Glucose Profile (AGP).
package stats

import (
	"math"
	"sort"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

// Consensus range boundaries in mg/dL. These are fixed by the standard so
// reports are comparable between people and clinics; personal alert
// thresholds are configured separately.
const (
	VeryLowBelow  = 54
	LowBelow      = 70
	HighAbove     = 180
	VeryHighAbove = 250
	TightHigh     = 140
)

// Ranges is the percentage of readings in each band.
type Ranges struct {
	VeryLow  float64 `json:"veryLow"`  // < 54
	Low      float64 `json:"low"`      // 54–69
	InRange  float64 `json:"inRange"`  // 70–180
	High     float64 `json:"high"`     // 181–250
	VeryHigh float64 `json:"veryHigh"` // > 250
	Tight    float64 `json:"tight"`    // 70–140 (time in tight range)
}

// Summary is the headline statistics for a period.
type Summary struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Days      float64   `json:"days"`
	Count     int       `json:"count"`
	Coverage  float64   `json:"coverage"` // % of expected 5-minute readings present
	Mean      float64   `json:"mean"`
	SD        float64   `json:"sd"`
	CV        float64   `json:"cv"`  // %
	GMI       float64   `json:"gmi"` // %
	Min       float64   `json:"min"`
	Max       float64   `json:"max"`
	Ranges    Ranges    `json:"ranges"`
	LowEvents int       `json:"lowEvents"` // episodes < 70 lasting >= 15 min
}

// AGPBucket holds percentiles for one time-of-day slot.
type AGPBucket struct {
	Minute int     `json:"minute"` // minutes after local midnight
	N      int     `json:"n"`
	P5     float64 `json:"p5"`
	P25    float64 `json:"p25"`
	P50    float64 `json:"p50"`
	P75    float64 `json:"p75"`
	P95    float64 `json:"p95"`
}

// Day is a per-day summary.
type Day struct {
	Date    string  `json:"date"` // YYYY-MM-DD local
	Count   int     `json:"count"`
	Mean    float64 `json:"mean"`
	InRange float64 `json:"inRange"`
	Low     float64 `json:"low"`  // % < 70
	High    float64 `json:"high"` // % > 180
}

// Report bundles everything the reports page shows.
type Report struct {
	Summary Summary     `json:"summary"`
	AGP     []AGPBucket `json:"agp"`
	Days    []Day       `json:"days"`
}

// Build computes a report for readings in [from, to), bucketing by time of
// day in loc.
func Build(rs []store.Reading, from, to time.Time, loc *time.Location) Report {
	return Report{
		Summary: Summarize(rs, from, to),
		AGP:     AGP(rs, loc, 15),
		Days:    Daily(rs, loc),
	}
}

// Summarize computes headline statistics.
func Summarize(rs []store.Reading, from, to time.Time) Summary {
	s := Summary{From: from, To: to, Days: to.Sub(from).Hours() / 24, Count: len(rs)}
	if expected := to.Sub(from).Minutes() / 5; expected > 0 {
		s.Coverage = math.Min(100, float64(len(rs))/expected*100)
	}
	if len(rs) == 0 {
		return s
	}
	var sum float64
	s.Min, s.Max = math.Inf(1), math.Inf(-1)
	var vl, l, ir, h, vh, tight int
	for _, r := range rs {
		v := r.Value
		sum += v
		s.Min = math.Min(s.Min, v)
		s.Max = math.Max(s.Max, v)
		switch {
		case v < VeryLowBelow:
			vl++
		case v < LowBelow:
			l++
		case v <= HighAbove:
			ir++
		case v <= VeryHighAbove:
			h++
		default:
			vh++
		}
		if v >= LowBelow && v <= TightHigh {
			tight++
		}
	}
	n := float64(len(rs))
	s.Mean = sum / n
	var ss float64
	for _, r := range rs {
		d := r.Value - s.Mean
		ss += d * d
	}
	if len(rs) > 1 {
		s.SD = math.Sqrt(ss / (n - 1))
	}
	s.CV = s.SD / s.Mean * 100
	s.GMI = 3.31 + 0.02392*s.Mean
	pct := func(c int) float64 { return float64(c) / n * 100 }
	s.Ranges = Ranges{pct(vl), pct(l), pct(ir), pct(h), pct(vh), pct(tight)}
	s.LowEvents = lowEvents(rs)
	return s
}

// lowEvents counts episodes below 70 mg/dL lasting at least 15 minutes.
// A gap of more than 15 minutes between readings ends an episode.
func lowEvents(rs []store.Reading) int {
	const minDur = 15 * 60 * 1000
	const maxGap = 15 * 60 * 1000
	events := 0
	var start, last int64 = -1, -1
	flush := func() {
		if start >= 0 && last-start >= minDur {
			events++
		}
		start = -1
	}
	for _, r := range rs {
		if start >= 0 && r.Time-last > maxGap {
			flush()
		}
		if r.Value < LowBelow {
			if start < 0 {
				start = r.Time
			}
			last = r.Time
		} else if start >= 0 {
			// The episode lasts until the first reading back in range.
			last = r.Time
			flush()
		}
	}
	flush()
	return events
}

// AGP computes time-of-day percentiles in slots of slotMinutes. Each slot
// draws on readings within ±30 minutes (wrapping around midnight), which
// smooths the curves the way AGP reports do.
func AGP(rs []store.Reading, loc *time.Location, slotMinutes int) []AGPBucket {
	const window = 30
	byMinute := make([][]float64, 1440)
	for _, r := range rs {
		t := time.UnixMilli(r.Time).In(loc)
		m := t.Hour()*60 + t.Minute()
		byMinute[m] = append(byMinute[m], r.Value)
	}
	slots := 1440 / slotMinutes
	out := make([]AGPBucket, 0, slots)
	vals := make([]float64, 0, 256)
	for i := 0; i < slots; i++ {
		center := i*slotMinutes + slotMinutes/2
		vals = vals[:0]
		for d := -window; d <= window; d++ {
			m := (center + d + 1440) % 1440
			vals = append(vals, byMinute[m]...)
		}
		b := AGPBucket{Minute: i * slotMinutes, N: len(vals)}
		if len(vals) >= 3 {
			sort.Float64s(vals)
			b.P5 = Percentile(vals, 5)
			b.P25 = Percentile(vals, 25)
			b.P50 = Percentile(vals, 50)
			b.P75 = Percentile(vals, 75)
			b.P95 = Percentile(vals, 95)
		}
		out = append(out, b)
	}
	return out
}

// Percentile returns the p-th percentile of sorted values using linear
// interpolation between closest ranks.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	frac := rank - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// Daily groups readings by local date.
func Daily(rs []store.Reading, loc *time.Location) []Day {
	type acc struct {
		n, ir, lo, hi int
		sum           float64
	}
	accs := map[string]*acc{}
	var keys []string
	for _, r := range rs {
		k := time.UnixMilli(r.Time).In(loc).Format("2006-01-02")
		a, ok := accs[k]
		if !ok {
			a = &acc{}
			accs[k] = a
			keys = append(keys, k)
		}
		a.n++
		a.sum += r.Value
		switch {
		case r.Value < LowBelow:
			a.lo++
		case r.Value <= HighAbove:
			a.ir++
		default:
			a.hi++
		}
	}
	sort.Strings(keys)
	out := make([]Day, 0, len(keys))
	for _, k := range keys {
		a := accs[k]
		n := float64(a.n)
		out = append(out, Day{
			Date: k, Count: a.n, Mean: a.sum / n,
			InRange: float64(a.ir) / n * 100,
			Low:     float64(a.lo) / n * 100,
			High:    float64(a.hi) / n * 100,
		})
	}
	return out
}
