package mock

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// Generation model. All local clock times are minutes after local midnight.
const (
	offWristChance   = 0.03 // whole day without data
	chargeGapChance  = 0.25 // one evening charging gap
	workoutChance    = 0.40
	napChance        = 0.12
	noSpO2Chance     = 0.08
	noHRVChance      = 0.04
	sessionSampleGap = 5 * time.Minute
)

// baseline is a user's stable physiology and habits.
type baseline struct {
	restingHR  float64 // bpm
	hrvMs      float64 // nightly RMSSD
	bmrKcal    float64 // basal kcal per day
	cadence    float64 // steps per minute while walking
	bedtimeMin float64 // relative to local midnight of the wake-up date (negative: evening before)
	sleepMin   float64 // typical time in bed
}

// dayPlan is everything decided once per local day. Times are absolute.
type dayPlan struct {
	date       domain.Date
	start, end time.Time // first instants of this date and the next
	offWrist   bool
	weekend    bool
	restingHR  float64
	hrvMs      float64
	activity   float64 // scales the chance of starting a walk
	gapStart   time.Time
	gapEnd     time.Time
	workStart  time.Time
	workEnd    time.Time
	main, nap  *sleepPlan // sessions waking on this date; nil if none
}

type sleepPlan struct {
	session domain.SleepSession
	hrvMs   float64
	noHRV   bool
	noSpO2  bool
}

func (s *sleepPlan) copySession() domain.SleepSession {
	out := s.session
	out.Stages = append([]domain.SleepStage(nil), s.session.Stages...)
	return out
}

// minuteSample is one fully worn minute of the per-minute series.
type minuteSample struct {
	at    time.Time
	hr    float64
	steps float64
	kcal  float64
}

func (m minuteSample) value(metric domain.MetricType) float64 {
	switch metric {
	case domain.MetricHeartRate:
		return m.hr
	case domain.MetricSteps:
		return m.steps
	default:
		return m.kcal
	}
}

// generator serves one request. It memoizes day plans because each day's
// series also needs the next day's sleep (which starts the evening before).
type generator struct {
	p     *Provider
	user  uuid.UUID
	loc   *time.Location
	now   time.Time
	base  baseline
	plans map[domain.Date]*dayPlan
}

func (p *Provider) newGenerator(conn domain.Connection) *generator {
	r := p.rng(conn.UserID, "baseline")
	return &generator{
		p: p, user: conn.UserID, loc: conn.Location, now: p.now(),
		base: baseline{
			restingHR:  uniform(r, 52, 66),
			hrvMs:      uniform(r, 35, 70),
			bmrKcal:    uniform(r, 1450, 1850),
			cadence:    uniform(r, 95, 115),
			bedtimeMin: uniform(r, -90, 0), // 22:30–00:00
			sleepMin:   uniform(r, 420, 480),
		},
		plans: map[domain.Date]*dayPlan{},
	}
}

func (g *generator) rng(d domain.Date, stream string) *rand.Rand {
	return g.p.rng(g.user, d.String()+"/"+stream)
}

func (g *generator) localTime(d domain.Date, minutes float64) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, int(minutes), 0, 0, g.loc)
}

func (g *generator) offWrist(d domain.Date) bool {
	return g.rng(d, "wear").Float64() < offWristChance
}

// completed reports whether s exists and has ended by now.
func (g *generator) completed(s *sleepPlan) bool {
	return s != nil && !s.session.EndAt.After(g.now)
}

func (g *generator) plan(d domain.Date) *dayPlan {
	if pl, ok := g.plans[d]; ok {
		return pl
	}
	pl := &dayPlan{date: d, start: d.StartIn(g.loc), end: d.AddDays(1).StartIn(g.loc), offWrist: g.offWrist(d)}
	g.plans[d] = pl
	if pl.offWrist {
		return pl
	}

	// Draw every value unconditionally and in a fixed order so a branch
	// never shifts the draws after it.
	r := g.rng(d, "day")
	rhrNoise, hrvNoise := r.NormFloat64(), r.NormFloat64()
	pl.activity = uniform(r, 0.6, 1.4)
	hasGap, gapAt, gapLen := r.Float64() < chargeGapChance, uniform(r, 17*60, 18*60+30), uniform(r, 45, 150)
	hasWork, workAt, workLen := r.Float64() < workoutChance, uniform(r, 11*60, 12*60+30), uniform(r, 30, 60)
	hasNap, napAt, napLen := r.Float64() < napChance, uniform(r, 14*60, 16*60), uniform(r, 20, 45)

	wd := d.Time().Weekday()
	pl.weekend = wd == time.Saturday || wd == time.Sunday

	// The watch must have been worn overnight for a main sleep to exist.
	if !g.offWrist(d.AddDays(-1)) {
		pl.main = g.mainSleep(d)
	}

	// Resting HR follows a slow 4-week cycle and rises after a short night;
	// HRV moves the opposite way.
	cycle := math.Sin(2 * math.Pi * float64(d.Time().Unix()/86400) / 28)
	pl.restingHR = g.base.restingHR + 2*cycle + 1.5*rhrNoise
	if pl.main != nil && pl.main.session.Duration() < 6*time.Hour {
		pl.restingHR += 2
	}
	pl.restingHR = clamp(pl.restingHR, 40, 95)
	pl.hrvMs = clamp(g.base.hrvMs-1.5*(pl.restingHR-g.base.restingHR)+4*hrvNoise, 10, 150)
	if pl.main != nil {
		pl.main.hrvMs = pl.hrvMs
	}

	if hasGap {
		pl.gapStart = g.localTime(d, gapAt)
		pl.gapEnd = pl.gapStart.Add(asMinutes(gapLen))
	}
	if hasWork {
		pl.workStart = g.localTime(d, workAt)
		pl.workEnd = pl.workStart.Add(asMinutes(workLen))
	}
	if hasNap {
		start := g.localTime(d, napAt)
		end := start.Add(asMinutes(napLen))
		nr := g.rng(d, "nap")
		var b stageBuilder
		b.reset(start, end)
		b.add(domain.SleepStageAwake, 1+nr.IntN(4))
		b.add(domain.SleepStageLight, int(napLen))
		pl.nap = &sleepPlan{session: g.session(d, "nap", start, end, false, b.stages), noHRV: true, noSpO2: true}
	}
	return pl
}

// mainSleep builds the night that ends on the morning of d.
func (g *generator) mainSleep(d domain.Date) *sleepPlan {
	r := g.rng(d, "sleep")
	bed := g.base.bedtimeMin + 25*r.NormFloat64()
	dur := g.base.sleepMin + 40*r.NormFloat64()
	noHRV, noSpO2 := r.Float64() < noHRVChance, r.Float64() < noSpO2Chance

	// Later and longer on Friday and Saturday nights.
	if wd := d.AddDays(-1).Time().Weekday(); wd == time.Friday || wd == time.Saturday {
		bed += 50
		dur += 30
	}
	// 21:30–01:30 bedtime and 5–9h20m in bed always wake up on d.
	bed = clamp(bed, -150, 90)
	dur = clamp(dur, 300, 560)

	start := g.localTime(d, bed)
	end := start.Add(asMinutes(dur))

	var b stageBuilder
	b.reset(start, end)
	if r.Float64() < 0.6 {
		b.add(domain.SleepStageAwake, 2+r.IntN(10)) // falling asleep
	}
	for c := 0.0; b.cursor.Before(end); c++ {
		before := b.cursor
		length := float64(85 + r.IntN(26))
		deep := math.Max(0.05, 0.28-0.06*c) // deep sleep early in the night
		rem := math.Min(0.35, 0.12+0.06*c)  // REM later
		light := 1 - deep - rem
		b.add(domain.SleepStageLight, int(math.Round(length*light/2)))
		b.add(domain.SleepStageDeep, int(math.Round(length*deep)))
		b.add(domain.SleepStageLight, int(math.Round(length*light/2)))
		b.add(domain.SleepStageREM, int(math.Round(length*rem)))
		if r.Float64() < 0.7 {
			b.add(domain.SleepStageAwake, 2+r.IntN(14)) // brief wake-up between cycles
		}
		if !b.cursor.After(before) {
			break
		}
	}
	return &sleepPlan{session: g.session(d, "main", start, end, true, b.stages), noHRV: noHRV, noSpO2: noSpO2}
}

func (g *generator) session(d domain.Date, kind string, start, end time.Time, main bool, stages []domain.SleepStage) domain.SleepSession {
	var total, awake time.Duration
	for _, st := range stages {
		total += st.EndAt.Sub(st.StartAt)
		if st.Stage == domain.SleepStageAwake {
			awake += st.EndAt.Sub(st.StartAt)
		}
	}
	eff := int16(math.Round(100 * float64(total-awake) / float64(total)))
	return domain.SleepSession{
		Provider:          domain.ProviderMock,
		ProviderSessionID: "mock-" + d.String() + "-" + kind,
		SleepDate:         d,
		StartAt:           start.UTC(),
		EndAt:             end.UTC(),
		IsMainSleep:       main,
		EfficiencyPct:     &eff,
		Stages:            stages,
	}
}

// stageBuilder appends contiguous whole-minute stages, merging repeats and
// truncating at the session end.
type stageBuilder struct {
	cursor, end time.Time
	stages      []domain.SleepStage
}

func (b *stageBuilder) reset(start, end time.Time) {
	b.cursor, b.end, b.stages = start.UTC(), end.UTC(), nil
}

func (b *stageBuilder) add(stage domain.SleepStageType, mins int) {
	if mins < 1 || !b.cursor.Before(b.end) {
		return
	}
	stop := b.cursor.Add(time.Duration(mins) * time.Minute)
	if stop.After(b.end) {
		stop = b.end
	}
	if n := len(b.stages); n > 0 && b.stages[n-1].Stage == stage {
		b.stages[n-1].EndAt = stop
	} else {
		b.stages = append(b.stages, domain.SleepStage{Stage: stage, StartAt: b.cursor, EndAt: stop})
	}
	b.cursor = stop
}

// stageHR offsets heart rate by sleep stage.
var stageHR = map[domain.SleepStageType]float64{
	domain.SleepStageDeep: -3, domain.SleepStageLight: 0, domain.SleepStageREM: 4, domain.SleepStageAwake: 6,
}

// minutes returns the per-minute series for local day d: one sample for every
// worn minute that has fully elapsed by now.
func (g *generator) minutes(d domain.Date) []minuteSample {
	pl := g.plan(d)
	if pl.offWrist {
		return nil
	}
	next := g.plan(d.AddDays(1)) // its main sleep starts this evening
	var sleeps []*sleepPlan
	for _, s := range []*sleepPlan{pl.main, pl.nap, next.main} {
		if s != nil {
			sleeps = append(sleeps, s)
		}
	}

	r := g.rng(d, "minutes")
	out := make([]minuteSample, 0, 1500)
	walking := false
	for t := pl.start; t.Before(pl.end); t = t.Add(time.Minute) {
		if t.Add(time.Minute).After(g.now) {
			break
		}
		stage, asleep := stageAt(sleeps, t)
		if !asleep && within(t, pl.gapStart, pl.gapEnd) {
			walking = false
			continue // charging: no samples
		}
		var hr, steps float64
		switch {
		case asleep:
			walking = false
			hr = pl.restingHR - 5 + stageHR[stage] + 1.2*r.NormFloat64()
		case within(t, pl.workStart, pl.workEnd):
			steps = math.Round(155 + 8*r.NormFloat64())
			hr = pl.restingHR + 80 + 8*r.NormFloat64()
		default:
			local := t.In(g.loc)
			hour := float64(local.Hour()) + float64(local.Minute())/60
			if walking {
				walking = r.Float64() < 0.8 // ~5-minute bouts
			} else {
				walking = r.Float64() < walkChance(hour, pl.weekend)*pl.activity
			}
			if walking {
				steps = math.Round(g.base.cadence + 8*r.NormFloat64())
			} else if r.Float64() < 0.2 {
				steps = float64(r.IntN(11)) // pottering about
			}
			hr = pl.restingHR + 14 + 0.3*steps + 3*r.NormFloat64()
		}
		steps = math.Max(0, steps)
		basal := g.base.bmrKcal / 1440
		if asleep {
			basal *= 0.92
		}
		out = append(out, minuteSample{
			at:    t.UTC(),
			hr:    math.Round(clamp(hr, 35, 200)),
			steps: steps,
			kcal:  round(basal+0.045*steps, 2),
		})
	}
	return out
}

// walkChance is the per-minute probability of starting a walk.
func walkChance(hour float64, weekend bool) float64 {
	switch {
	case hour < 6 || hour >= 23:
		return 0.002
	case weekend && hour >= 10 && hour < 17:
		return 0.02
	case !weekend && (hour >= 7.5 && hour < 9 || hour >= 16.5 && hour < 18): // commute
		return 0.035
	case hour >= 12 && hour < 13: // lunch
		return 0.025
	default:
		return 0.006
	}
}

// sessionSamples returns the HRV or SpO2 samples of the completed main sleep
// waking on d, every sessionSampleGap.
func (g *generator) sessionSamples(d domain.Date, metric domain.MetricType) []domain.IntradayPoint {
	s := g.plan(d).main
	if !g.completed(s) || (metric == domain.MetricHRVRMSSD && s.noHRV) || (metric == domain.MetricSpO2 && s.noSpO2) {
		return nil
	}
	r := g.rng(d, string(metric))
	var out []domain.IntradayPoint
	for t := s.session.StartAt.Add(sessionSampleGap); t.Before(s.session.EndAt); t = t.Add(sessionSampleGap) {
		var v float64
		if metric == domain.MetricHRVRMSSD {
			v = round(clamp(s.hrvMs+7*r.NormFloat64(), 5, 250), 1)
		} else {
			v = round(clamp(96.2+r.NormFloat64(), 88, 100), 1)
		}
		out = append(out, domain.IntradayPoint{Metric: metric, RecordedAt: t, Value: v})
	}
	return out
}

// summary aggregates local day d. It reports false when d has no data.
func (g *generator) summary(d domain.Date) (domain.DailySummary, bool) {
	mins := g.minutes(d)
	if len(mins) == 0 {
		return domain.DailySummary{}, false
	}
	var steps, kcal float64
	for _, m := range mins {
		steps += m.steps
		kcal += m.kcal
	}
	pl := g.plan(d)
	s := domain.DailySummary{
		Provider:         domain.ProviderMock,
		Date:             d,
		Steps:            ptr(int32(steps)),
		CaloriesKcal:     ptr(int32(math.Round(kcal))),
		RestingHeartRate: ptr(int16(math.Round(pl.restingHR))),
	}
	if g.completed(pl.main) {
		s.SleepScore = ptr(sleepScore(pl.main.session))
		s.HRVRMSSDMs = mean(g.sessionSamples(d, domain.MetricHRVRMSSD))
		s.SpO2AvgPct = mean(g.sessionSamples(d, domain.MetricSpO2))
	}
	return s, true
}

// sleepScore blends duration (50), deep+REM share (25) and efficiency (25).
func sleepScore(s domain.SleepSession) int16 {
	var asleep, restorative time.Duration
	for _, st := range s.Stages {
		dur := st.EndAt.Sub(st.StartAt)
		if st.Stage != domain.SleepStageAwake {
			asleep += dur
		}
		if st.Stage == domain.SleepStageDeep || st.Stage == domain.SleepStageREM {
			restorative += dur
		}
	}
	if asleep == 0 {
		return 0
	}
	eff := float64(asleep) / float64(s.Duration())
	score := 50*clamp(asleep.Hours()/8, 0, 1) +
		25*clamp(float64(restorative)/float64(asleep)/0.45, 0, 1) +
		25*clamp((eff-0.75)/0.20, 0, 1)
	return int16(math.Round(clamp(score, 0, 100)))
}

func stageAt(sleeps []*sleepPlan, t time.Time) (domain.SleepStageType, bool) {
	for _, s := range sleeps {
		if !within(t, s.session.StartAt, s.session.EndAt) {
			continue
		}
		for _, st := range s.session.Stages {
			if within(t, st.StartAt, st.EndAt) {
				return st.Stage, true
			}
		}
	}
	return "", false
}

// within reports whether t is in [start, end). Zero bounds never match.
func within(t, start, end time.Time) bool { return !t.Before(start) && t.Before(end) }

func mean(pts []domain.IntradayPoint) *float64 {
	if len(pts) == 0 {
		return nil
	}
	var sum float64
	for _, p := range pts {
		sum += p.Value
	}
	return ptr(round(sum/float64(len(pts)), 1))
}

func uniform(r *rand.Rand, lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func round(v float64, decimals int) float64 {
	f := math.Pow(10, float64(decimals))
	return math.Round(v*f) / f
}

func asMinutes(m float64) time.Duration { return time.Duration(m) * time.Minute }

func ptr[T any](v T) *T { return &v }
