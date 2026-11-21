package sync

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// cursorKey is the orchestrator's key in the provider_connections.sync_cursor
// JSON object. Other keys belong to provider adapters and are preserved.
const cursorKey = "synced_through"

// cursor is a connection's sync_cursor: the raw JSON plus the parsed last
// local day that a scheduled or manual run fully ingested.
type cursor struct {
	raw           []byte
	fields        map[string]json.RawMessage
	syncedThrough *domain.Date
}

func parseCursor(raw []byte) (cursor, error) {
	c := cursor{raw: raw, fields: map[string]json.RawMessage{}}
	if len(raw) == 0 {
		c.raw = []byte("{}")
		return c, nil
	}
	if err := json.Unmarshal(raw, &c.fields); err != nil {
		return cursor{}, fmt.Errorf("parsing sync cursor: %w", err)
	}
	if v, ok := c.fields[cursorKey]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return cursor{}, fmt.Errorf("parsing sync cursor %s: %w", cursorKey, err)
		}
		d, err := domain.ParseDate(s)
		if err != nil {
			return cursor{}, fmt.Errorf("parsing sync cursor: %w", err)
		}
		c.syncedThrough = &d
	}
	return c, nil
}

// advance returns the cursor JSON moved forward to through. It never moves
// backwards, so an older range synced manually cannot rewind the schedule.
func (c cursor) advance(through domain.Date) ([]byte, error) {
	if c.syncedThrough != nil && through.Before(*c.syncedThrough) {
		through = *c.syncedThrough
	}
	fields := make(map[string]json.RawMessage, len(c.fields)+1)
	for k, v := range c.fields {
		fields[k] = v
	}
	v, err := json.Marshal(through.String())
	if err != nil {
		return nil, fmt.Errorf("encoding sync cursor: %w", err)
	}
	fields[cursorKey] = v
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding sync cursor: %w", err)
	}
	return raw, nil
}

// planRange decides which local days a run covers. Backfills use the
// requested range, capped at today. Scheduled and manual runs resume
// OverlapDays before the cursor (or InitialBackfillDays back on a
// connection's first sync) and end today, whose data is still partial and
// will be re-fetched by the next run's overlap.
func planRange(req Request, syncedThrough *domain.Date, today domain.Date, cfg Config) (domain.DateRange, error) {
	if req.Range != nil {
		r := *req.Range
		if today.Before(r.Start) {
			return domain.DateRange{}, fmt.Errorf("backfill starts %s, after today %s", r.Start, today)
		}
		if today.Before(r.End) {
			r.End = today
		}
		return r, nil
	}
	start := today.AddDays(-(cfg.InitialBackfillDays - 1))
	if syncedThrough != nil {
		start = syncedThrough.AddDays(-cfg.OverlapDays)
	}
	if today.Before(start) {
		start = today
	}
	return domain.DateRange{Start: start, End: today}, nil
}

// chunkDates splits r into consecutive ranges of at most n days.
func chunkDates(r domain.DateRange, n int) []domain.DateRange {
	var out []domain.DateRange
	for start := r.Start; !r.End.Before(start); start = start.AddDays(n) {
		end := start.AddDays(n - 1)
		if r.End.Before(end) {
			end = r.End
		}
		out = append(out, domain.DateRange{Start: start, End: end})
	}
	return out
}

// localSpan is the instant range covering local days r in loc:
// [midnight of r.Start, midnight after r.End).
func localSpan(r domain.DateRange, loc *time.Location) domain.TimeRange {
	return domain.TimeRange{
		Start: r.Start.StartIn(loc),
		End:   r.End.AddDays(1).StartIn(loc),
	}
}

// nextNightly returns the next occurrence of hour:00 local time after now.
func nextNightly(now time.Time, loc *time.Location, hour int) time.Time {
	l := now.In(loc)
	next := time.Date(l.Year(), l.Month(), l.Day(), hour, 0, 0, 0, loc)
	if !next.After(now) {
		next = time.Date(l.Year(), l.Month(), l.Day()+1, hour, 0, 0, 0, loc)
	}
	return next.UTC()
}

func rangeString(r domain.DateRange) string {
	if r.Start == r.End {
		return r.Start.String()
	}
	return r.Start.String() + ".." + r.End.String()
}
