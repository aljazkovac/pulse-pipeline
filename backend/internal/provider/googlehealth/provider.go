// Package googlehealth implements domain.HealthProvider for the Google Health
// API v4 (developers.google.com/health), the successor to the Fitbit Web API
// and the source of Fitbit Air data (ADR-0004).
//
// It maps Google's JSON to Pulse domain types only; no vendor type leaves the
// package. Failures wrap domain.ErrTransient (429, 5xx, timeouts) or
// domain.ErrUnauthorized (401, revoked refresh token) so the sync worker can
// react without knowing the vendor. Errors never contain tokens or raw
// response bodies.
package googlehealth

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// Production endpoints.
const (
	DefaultAPIBaseURL = "https://health.googleapis.com/v4"
	DefaultAuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL   = "https://oauth2.googleapis.com/token"
	// DefaultTimeout bounds every HTTP request, including reading the body.
	DefaultTimeout = 30 * time.Second
)

var scopes = []string{
	"https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly",
	"https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly",
	"https://www.googleapis.com/auth/googlehealth.sleep.readonly",
}

// Scopes returns the read-only scopes Pulse requests. Every one is required:
// ADR-0004 §3 explains why partial consent is rejected at link time.
func Scopes() []string { return slices.Clone(scopes) }

// API limits (developers.google.com/health/aggregate-data, /reference/rest).
const (
	maxPageSize          = 10000 // list, reconcile, rollUp
	sleepPageSize        = 25    // list/reconcile for sleep
	rollupMaxDays        = 90    // rollUp/dailyRollUp for most types
	rollupMaxDaysShort   = 14    // ... for total-calories and heart-rate
	maxPagesPerQuery     = 1000  // runaway-pagination guard
	maxResponseBodyBytes = 64 << 20
)

// Options configures a Provider. ClientID is required.
type Options struct {
	ClientID     string
	ClientSecret string
	// APIBaseURL, AuthURL and TokenURL default to Google's production
	// endpoints; tests point them at an httptest server.
	APIBaseURL string
	AuthURL    string
	TokenURL   string
	// HTTPClient defaults to a client with DefaultTimeout. A supplied client
	// must have a non-zero Timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now; it dates token expiry.
	Now func() time.Time
	// LoopbackTimeout bounds AuthorizeLoopback; default 5 minutes.
	LoopbackTimeout time.Duration
}

// Provider talks to the Google Health API. It is safe for concurrent use.
type Provider struct {
	clientID, clientSecret     string
	apiBase, authURL, tokenURL string
	hc                         *http.Client
	now                        func() time.Time
	loopbackTimeout            time.Duration
}

var _ domain.HealthProvider = (*Provider)(nil)

// New returns a Provider.
func New(opts Options) (*Provider, error) {
	if opts.ClientID == "" {
		return nil, errors.New("googlehealth: client ID is required")
	}
	p := &Provider{
		clientID: opts.ClientID, clientSecret: opts.ClientSecret,
		apiBase: strings.TrimRight(cmp.Or(opts.APIBaseURL, DefaultAPIBaseURL), "/"),
		authURL: cmp.Or(opts.AuthURL, DefaultAuthURL), tokenURL: cmp.Or(opts.TokenURL, DefaultTokenURL),
		hc: opts.HTTPClient, now: opts.Now, loopbackTimeout: opts.LoopbackTimeout,
	}
	if p.hc == nil {
		p.hc = &http.Client{
			Timeout: DefaultTimeout,
			// Never follow redirects: a redirected token request would re-send
			// the client secret and refresh token to another URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	} else if p.hc.Timeout <= 0 {
		return nil, errors.New("googlehealth: HTTP client needs a timeout")
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.loopbackTimeout <= 0 {
		p.loopbackTimeout = 5 * time.Minute
	}
	return p, nil
}

// String redacts the client secret, so printing a Provider cannot leak it.
func (p *Provider) String() string { return fmt.Sprintf("googlehealth.Provider{client_id: %q}", p.clientID) }

// GoString redacts the client secret for %#v as well.
func (p *Provider) GoString() string { return p.String() }

// Name returns domain.ProviderGoogleHealth.
func (p *Provider) Name() domain.Provider { return domain.ProviderGoogleHealth }

// ---------------------------------------------------------------------------
// Daily summaries
// ---------------------------------------------------------------------------

// FetchDailySummaries returns one summary per civil day in r with at least
// one metric: steps and calories from daily rollups, resting heart rate,
// HRV (RMSSD) and SpO2 from Google's daily-* data types. Sleep score is not
// offered by the API and stays nil.
func (p *Provider) FetchDailySummaries(ctx context.Context, conn domain.Connection, r domain.DateRange) ([]domain.DailySummary, error) {
	wrap := func(err error) error {
		return fmt.Errorf("googlehealth: fetch daily summaries for user %s: %w", conn.UserID, err)
	}
	if err := checkRequest(ctx, conn); err != nil {
		return nil, wrap(err)
	}
	if err := r.Validate(); err != nil {
		return nil, wrap(err)
	}

	days := map[domain.Date]*domain.DailySummary{}
	day := func(d domain.Date) *domain.DailySummary {
		s, ok := days[d]
		if !ok {
			s = &domain.DailySummary{Provider: domain.ProviderGoogleHealth, Date: d}
			days[d] = s
		}
		return s
	}
	inRange := func(d domain.Date) bool { return !d.Before(r.Start) && !r.End.Before(d) }

	if err := p.dailyRollUp(ctx, conn, "steps", r, rollupMaxDays, func(d domain.Date, pt rollupPoint) {
		if pt.Steps != nil && pt.Steps.CountSum != nil && inRange(d) {
			if v := int64(*pt.Steps.CountSum); v >= 0 && v <= math.MaxInt32 {
				day(d).Steps = ptr(int32(v))
			}
		}
	}); err != nil {
		return nil, wrap(fmt.Errorf("steps: %w", err))
	}
	if err := p.dailyRollUp(ctx, conn, "total-calories", r, rollupMaxDaysShort, func(d domain.Date, pt rollupPoint) {
		if pt.TotalCalories != nil && pt.TotalCalories.KcalSum != nil && inRange(d) {
			if v := math.Round(*pt.TotalCalories.KcalSum); v >= 0 && v <= math.MaxInt32 {
				day(d).CaloriesKcal = ptr(int32(v))
			}
		}
	}); err != nil {
		return nil, wrap(fmt.Errorf("total calories: %w", err))
	}

	type daily struct {
		path, filter string
		set          func(dataPoint)
	}
	for _, t := range []daily{
		{"daily-resting-heart-rate", "daily_resting_heart_rate", func(dp dataPoint) {
			v := dp.DailyRestingHeartRate
			if v == nil || v.BeatsPerMinute == nil {
				return
			}
			// Schema CHECK: resting_heart_rate BETWEEN 20 AND 250.
			if d, ok := v.Date.date(); ok && inRange(d) && *v.BeatsPerMinute >= 20 && *v.BeatsPerMinute <= 250 {
				day(d).RestingHeartRate = ptr(int16(*v.BeatsPerMinute))
			}
		}},
		{"daily-heart-rate-variability", "daily_heart_rate_variability", func(dp dataPoint) {
			v := dp.DailyHeartRateVariability
			if v == nil || v.AverageMs == nil {
				return
			}
			if d, ok := v.Date.date(); ok && inRange(d) && *v.AverageMs >= 0 && !math.IsInf(*v.AverageMs, 0) {
				day(d).HRVRMSSDMs = ptr(*v.AverageMs)
			}
		}},
		{"daily-oxygen-saturation", "daily_oxygen_saturation", func(dp dataPoint) {
			v := dp.DailyOxygenSaturation
			if v == nil || v.AveragePercentage == nil {
				return
			}
			if d, ok := v.Date.date(); ok && inRange(d) && *v.AveragePercentage >= 0 && *v.AveragePercentage <= 100 {
				day(d).SpO2AvgPct = ptr(*v.AveragePercentage)
			}
		}},
	} {
		filter := fmt.Sprintf(`%s.date >= "%s" AND %s.date < "%s"`, t.filter, r.Start, t.filter, r.End.AddDays(1))
		if err := p.reconcile(ctx, conn, t.path, filter, maxPageSize, t.set); err != nil {
			return nil, wrap(fmt.Errorf("%s: %w", t.path, err))
		}
	}

	out := make([]domain.DailySummary, 0, len(days))
	for _, s := range days {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b domain.DailySummary) int { return a.Date.Time().Compare(b.Date.Time()) })
	return out, nil
}

// dailyRollUp requests one-day windows over r in calls of at most maxDays and
// calls fn with each window's civil start date.
func (p *Provider) dailyRollUp(ctx context.Context, conn domain.Connection, dataType string, r domain.DateRange, maxDays int, fn func(domain.Date, rollupPoint)) error {
	for start := r.Start; !r.End.Before(start); start = start.AddDays(maxDays) {
		end := start.AddDays(maxDays - 1)
		if r.End.Before(end) {
			end = r.End
		}
		var req dailyRollUpRequest
		req.Range.Start = civilDateTime{Date: toCivilDate(start)}
		req.Range.End = civilDateTime{Date: toCivilDate(end.AddDays(1))} // exclusive
		req.WindowSizeDays = 1
		req.PageSize = daysIn(start, end)
		err := paginate(func(token string) (string, error) {
			req.PageToken = token
			var resp rollupResponse
			if err := p.postJSON(ctx, conn, dataPointsPath(dataType)+":dailyRollUp", req, &resp); err != nil {
				return "", err
			}
			for _, pt := range resp.RollupDataPoints {
				if pt.CivilStartTime == nil {
					continue
				}
				if d, ok := pt.CivilStartTime.Date.date(); ok {
					fn(d, pt)
				}
			}
			return resp.NextPageToken, nil
		})
		if err != nil {
			return fmt.Errorf("daily rollup %s..%s: %w", start, end, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Intraday metrics
// ---------------------------------------------------------------------------

// FetchIntradayMetrics returns samples of metric with RecordedAt in r. Heart
// rate, HRV and SpO2 are sensor samples; steps are per-interval counts at the
// interval start (a returned interval without a count is a worn-but-idle
// minute, i.e. 0); calories are 60-second rollup windows (unworn windows are
// skipped).
func (p *Provider) FetchIntradayMetrics(ctx context.Context, conn domain.Connection, metric domain.MetricType, r domain.TimeRange) ([]domain.IntradayPoint, error) {
	wrap := func(err error) error {
		return fmt.Errorf("googlehealth: fetch %s metrics for user %s: %w", metric, conn.UserID, err)
	}
	if err := checkRequest(ctx, conn); err != nil {
		return nil, wrap(err)
	}
	if err := r.Validate(); err != nil {
		return nil, wrap(err)
	}

	out := []domain.IntradayPoint{}
	add := func(at time.Time, v float64) {
		if !at.Before(r.Start) && at.Before(r.End) && !math.IsNaN(v) && !math.IsInf(v, 0) {
			out = append(out, domain.IntradayPoint{Metric: metric, RecordedAt: at.UTC(), Value: v})
		}
	}
	samples := func(path, filterType string, value func(dataPoint) (time.Time, float64, bool)) error {
		field := filterType + ".sample_time.physical_time"
		return p.reconcile(ctx, conn, path, physicalFilter(field, r), maxPageSize, func(dp dataPoint) {
			if at, v, ok := value(dp); ok {
				add(at, v)
			}
		})
	}

	var err error
	switch metric {
	case domain.MetricHeartRate:
		err = samples("heart-rate", "heart_rate", func(dp dataPoint) (time.Time, float64, bool) {
			if v := dp.HeartRate; v != nil && v.BeatsPerMinute != nil {
				return v.SampleTime.PhysicalTime, float64(*v.BeatsPerMinute), true
			}
			return time.Time{}, 0, false
		})
	case domain.MetricHRVRMSSD:
		err = samples("heart-rate-variability", "heart_rate_variability", func(dp dataPoint) (time.Time, float64, bool) {
			if v := dp.HeartRateVariability; v != nil && v.RMSSDMs != nil {
				return v.SampleTime.PhysicalTime, *v.RMSSDMs, true
			}
			return time.Time{}, 0, false
		})
	case domain.MetricSpO2:
		err = samples("oxygen-saturation", "oxygen_saturation", func(dp dataPoint) (time.Time, float64, bool) {
			if v := dp.OxygenSaturation; v != nil && v.Percentage != nil {
				return v.SampleTime.PhysicalTime, *v.Percentage, true
			}
			return time.Time{}, 0, false
		})
	case domain.MetricSteps:
		err = p.reconcile(ctx, conn, "steps", physicalFilter("steps.interval.start_time", r), maxPageSize, func(dp dataPoint) {
			if v := dp.Steps; v != nil {
				count := 0.0 // a returned interval without a count is a true zero
				if v.Count != nil {
					count = float64(*v.Count)
				}
				add(v.Interval.StartTime, count)
			}
		})
	case domain.MetricCalories:
		err = p.rollUp(ctx, conn, "total-calories", r, rollupMaxDaysShort, func(pt rollupPoint) {
			if pt.TotalCalories != nil && pt.TotalCalories.KcalSum != nil {
				add(pt.StartTime, *pt.TotalCalories.KcalSum)
			}
		})
	default:
		err = fmt.Errorf("unknown metric type %q", metric)
	}
	if err != nil {
		return nil, wrap(err)
	}
	return out, nil
}

// rollUp requests 60-second windows over r in calls of at most maxDays.
func (p *Provider) rollUp(ctx context.Context, conn domain.Connection, dataType string, r domain.TimeRange, maxDays int, fn func(rollupPoint)) error {
	step := time.Duration(maxDays) * 24 * time.Hour
	for start := r.Start; start.Before(r.End); start = start.Add(step) {
		end := start.Add(step)
		if end.After(r.End) {
			end = r.End
		}
		var req rollUpRequest
		req.Range.StartTime = start.UTC().Format(time.RFC3339Nano)
		req.Range.EndTime = end.UTC().Format(time.RFC3339Nano)
		req.WindowSize = "60s"
		req.PageSize = maxPageSize
		err := paginate(func(token string) (string, error) {
			req.PageToken = token
			var resp rollupResponse
			if err := p.postJSON(ctx, conn, dataPointsPath(dataType)+":rollUp", req, &resp); err != nil {
				return "", err
			}
			for _, pt := range resp.RollupDataPoints {
				fn(pt)
			}
			return resp.NextPageToken, nil
		})
		if err != nil {
			return fmt.Errorf("rollup %s..%s: %w", req.Range.StartTime, req.Range.EndTime, err)
		}
	}
	return nil
}

// physicalFilter selects field in [r.Start, r.End). The API accepts only >=
// and <, which is exactly a half-open range.
func physicalFilter(field string, r domain.TimeRange) string {
	return fmt.Sprintf(`%s >= "%s" AND %s < "%s"`,
		field, r.Start.UTC().Format(time.RFC3339Nano), field, r.End.UTC().Format(time.RFC3339Nano))
}

// ---------------------------------------------------------------------------
// Sleep
// ---------------------------------------------------------------------------

// FetchSleepSessions returns sessions whose wake-up date in conn.Location is
// in r. Google filters sleep by civil end time only, using the device's UTC
// offset, so the query is widened by a day on each side and the local date
// decides.
func (p *Provider) FetchSleepSessions(ctx context.Context, conn domain.Connection, r domain.DateRange) ([]domain.SleepSession, error) {
	wrap := func(err error) error {
		return fmt.Errorf("googlehealth: fetch sleep sessions for user %s: %w", conn.UserID, err)
	}
	if err := checkRequest(ctx, conn); err != nil {
		return nil, wrap(err)
	}
	if err := r.Validate(); err != nil {
		return nil, wrap(err)
	}
	filter := fmt.Sprintf(`sleep.interval.civil_end_time >= "%s" AND sleep.interval.civil_end_time < "%s"`,
		r.Start.AddDays(-1), r.End.AddDays(2))
	out := []domain.SleepSession{}
	seen := map[string]bool{}
	err := p.reconcile(ctx, conn, "sleep", filter, sleepPageSize, func(dp dataPoint) {
		s, ok := mapSleep(dp, conn.Location)
		if !ok || seen[s.ProviderSessionID] || s.SleepDate.Before(r.Start) || r.End.Before(s.SleepDate) {
			return
		}
		seen[s.ProviderSessionID] = true
		out = append(out, s)
	})
	if err != nil {
		return nil, wrap(err)
	}
	slices.SortFunc(out, func(a, b domain.SleepSession) int { return a.StartAt.Compare(b.StartAt) })
	return out, nil
}

// stageTypes maps STAGES sessions' segments. CLASSIC sessions (ASLEEP,
// RESTLESS, AWAKE) get no stages at all: their asleep time has no faithful
// Pulse stage, and keeping only the AWAKE segments would read as a night
// spent awake (ADR-0004 §1). Unknown segment types are skipped.
var stageTypes = map[string]domain.SleepStageType{
	"DEEP":  domain.SleepStageDeep,
	"REM":   domain.SleepStageREM,
	"LIGHT": domain.SleepStageLight,
	"AWAKE": domain.SleepStageAwake,
}

// mapSleep converts one reconciled sleep data point. It reports false for
// points that are not usable sessions (no ID, empty interval).
func mapSleep(dp dataPoint, loc *time.Location) (domain.SleepSession, bool) {
	sv := dp.Sleep
	if sv == nil {
		return domain.SleepSession{}, false
	}
	id := sessionID(cmp.Or(dp.DataPointName, dp.Name))
	start, end := sv.Interval.StartTime.UTC(), sv.Interval.EndTime.UTC()
	if id == "" || !end.After(start) {
		return domain.SleepSession{}, false
	}
	s := domain.SleepSession{
		Provider:          domain.ProviderGoogleHealth,
		ProviderSessionID: id,
		SleepDate:         domain.DateOf(end.In(loc)),
		StartAt:           start,
		EndAt:             end,
		IsMainSleep:       sv.Metadata.MainSleep,
		Stages:            []domain.SleepStage{},
	}
	if a, b := sv.Summary.MinutesAsleep, sv.Summary.MinutesInSleepPeriod; a != nil && b != nil && *b > 0 && *a >= 0 {
		// Google's documented formula: round(minutes asleep / minutes in bed × 100).
		eff := math.Round(float64(*a) / float64(*b) * 100)
		s.EfficiencyPct = ptr(int16(min(eff, 100)))
	}
	if sv.Type == "CLASSIC" {
		return s, true
	}
	starts := map[int64]bool{}
	for _, st := range sv.Stages {
		typ, ok := stageTypes[st.Type]
		stStart, stEnd := st.StartTime.UTC(), st.EndTime.UTC()
		key := stStart.Truncate(time.Microsecond).UnixMicro()
		if !ok || !stEnd.After(stStart) || starts[key] {
			continue
		}
		starts[key] = true
		s.Stages = append(s.Stages, domain.SleepStage{Stage: typ, StartAt: stStart, EndAt: stEnd})
	}
	slices.SortFunc(s.Stages, func(a, b domain.SleepStage) int { return a.StartAt.Compare(b.StartAt) })
	return s, true
}

// sessionID is the last segment of users/{user}/dataTypes/sleep/dataPoints/{id},
// stable whether the user segment is "me" or the numeric health user ID.
func sessionID(name string) string {
	const marker = "/dataPoints/"
	if i := strings.LastIndex(name, marker); i >= 0 {
		return name[i+len(marker):]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Auth refresh
// ---------------------------------------------------------------------------

// RefreshAuth exchanges the connection's refresh token for a new access
// token. Google may rotate the refresh token; otherwise the old one is kept.
func (p *Provider) RefreshAuth(ctx context.Context, conn domain.Connection) (domain.Credentials, error) {
	wrap := func(err error) error {
		return fmt.Errorf("googlehealth: refresh auth for user %s: %w", conn.UserID, err)
	}
	if err := ctx.Err(); err != nil {
		return domain.Credentials{}, wrap(err)
	}
	if conn.Credentials.RefreshToken == "" {
		return domain.Credentials{}, wrap(fmt.Errorf("%w: no refresh token stored; re-link with pulse-worker connect-google", domain.ErrUnauthorized))
	}
	tok, err := p.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {conn.Credentials.RefreshToken},
	})
	if err != nil {
		return domain.Credentials{}, wrap(err)
	}
	creds := p.credentials(tok)
	if creds.RefreshToken == "" {
		creds.RefreshToken = conn.Credentials.RefreshToken
	}
	return creds, nil
}

// token posts a grant to the token endpoint, adding the client credentials.
func (p *Provider) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	form.Set("client_id", p.clientID)
	if p.clientSecret != "" {
		form.Set("client_secret", p.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	code, body, err := p.roundTrip(ctx, req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint: %w", err)
	}
	if code != http.StatusOK {
		return tokenResponse{}, newTokenError(code, body)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, errors.New("token endpoint: malformed response") // the body holds tokens; never echo it
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, errors.New("token endpoint: response has no access token")
	}
	return tok, nil
}

func (p *Provider) credentials(tok tokenResponse) domain.Credentials {
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour // Google's documented lifetime; zero would mean "never refresh"
	}
	return domain.Credentials{
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		ExpiresAt: p.now().Add(ttl).UTC(),
	}
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func checkRequest(ctx context.Context, conn domain.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case conn.UserID == uuid.Nil:
		return errors.New("connection user ID is required")
	case conn.Location == nil:
		return errors.New("connection location is required")
	case conn.Provider != domain.ProviderGoogleHealth:
		return fmt.Errorf("connection provider %q is not %q", conn.Provider, domain.ProviderGoogleHealth)
	case conn.Credentials.AccessToken == "":
		return fmt.Errorf("%w: no access token stored; link the account with pulse-worker connect-google", domain.ErrUnauthorized)
	}
	return nil
}

func dataPointsPath(dataType string) string {
	return "/users/me/dataTypes/" + dataType + "/dataPoints"
}

// reconcile pages through a :reconcile query, calling fn per data point.
func (p *Provider) reconcile(ctx context.Context, conn domain.Connection, dataType, filter string, pageSize int, fn func(dataPoint)) error {
	return paginate(func(token string) (string, error) {
		q := url.Values{"filter": {filter}, "pageSize": {strconv.Itoa(pageSize)}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var resp pageResponse
		if err := p.getJSON(ctx, conn, dataPointsPath(dataType)+":reconcile", q, &resp); err != nil {
			return "", err
		}
		for _, dp := range resp.DataPoints {
			fn(dp)
		}
		return resp.NextPageToken, nil
	})
}

// paginate calls fetch with each page token until a page has no next token.
func paginate(fetch func(token string) (next string, err error)) error {
	seen := map[string]bool{}
	token := ""
	for range maxPagesPerQuery {
		next, err := fetch(token)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if seen[next] {
			return errors.New("pagination did not advance (repeated page token)")
		}
		seen[next] = true
		token = next
	}
	return fmt.Errorf("more than %d pages", maxPagesPerQuery)
}

func (p *Provider) getJSON(ctx context.Context, conn domain.Connection, path string, q url.Values, out any) error {
	u := p.apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	return p.call(ctx, conn.Credentials.AccessToken, req, out)
}

func (p *Provider) postJSON(ctx context.Context, conn domain.Connection, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return p.call(ctx, conn.Credentials.AccessToken, req, out)
}

// call sends an authenticated API request and decodes a 200 response.
func (p *Provider) call(ctx context.Context, accessToken string, req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	code, body, err := p.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return newAPIError(code, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		// Go's JSON errors can quote values (health readings, timestamps);
		// keep only where decoding failed.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return fmt.Errorf("decoding response: unexpected type at %s", typeErr.Field)
		}
		return errors.New("decoding response: malformed JSON")
	}
	return nil
}

func (p *Provider) roundTrip(ctx context.Context, req *http.Request) (int, []byte, error) {
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, transportError(ctx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; nothing to report
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return 0, nil, transportError(ctx, err)
	}
	return resp.StatusCode, body, nil
}

func daysIn(start, end domain.Date) int {
	return int(end.Time().Sub(start.Time()).Hours()/24) + 1
}

func ptr[T any](v T) *T { return &v }
