package domain

import (
	"context"
	"errors"
	"log/slog"
	"time"
	_ "time/tzdata" // embed zoneinfo so user timezones resolve on minimal container images

	"github.com/google/uuid"
)

// User is a Pulse Pipeline account.
type User struct {
	ID        uuid.UUID
	Email     string
	Timezone  string // IANA name, e.g. "Europe/Stockholm"
	CreatedAt time.Time
}

// Location loads the user's timezone, falling back to UTC for unknown names.
func (u User) Location() *time.Location {
	loc, err := time.LoadLocation(u.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ConnectionStatus is the lifecycle state of a provider connection.
type ConnectionStatus string

const (
	ConnectionActive  ConnectionStatus = "active"
	ConnectionRevoked ConnectionStatus = "revoked"
	ConnectionError   ConnectionStatus = "error"
)

// Credentials are decrypted OAuth tokens. They exist only in memory inside the
// sync worker and provider adapters; never log or serialize them.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// String redacts the tokens so accidental %v logging cannot leak them.
func (Credentials) String() string { return "domain.Credentials{REDACTED}" }

// GoString redacts the tokens for %#v as well.
func (Credentials) GoString() string { return "domain.Credentials{REDACTED}" }

// MarshalJSON redacts the tokens if Credentials are ever JSON-encoded.
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"REDACTED"`), nil }

// LogValue redacts the tokens in structured (slog) logs.
func (Credentials) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// Connection is what a HealthProvider needs to fetch one user's data.
type Connection struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Provider    Provider
	Location    *time.Location // user's timezone for day boundaries
	Credentials Credentials
}

// Errors HealthProvider implementations wrap so the sync worker can react
// without knowing the vendor (ADR-0003):
var (
	// ErrTransient marks a failure worth retrying with backoff: rate limiting
	// (HTTP 429), upstream unavailability (5xx) or a network timeout.
	ErrTransient = errors.New("transient provider error")
	// ErrUnauthorized means the provider rejected the access token during a
	// fetch or the refresh token during RefreshAuth.
	ErrUnauthorized = errors.New("provider rejected credentials")
)

// HealthProvider is implemented by every upstream wearable integration
// (provider/mock, provider/googlehealth). Implementations return normalized
// domain values only; raw vendor payloads never leave the adapter.
type HealthProvider interface {
	// Name identifies the provider; it is stored in every ingested row.
	Name() Provider
	// FetchDailySummaries returns one summary per local day in r that has data.
	FetchDailySummaries(ctx context.Context, conn Connection, r DateRange) ([]DailySummary, error)
	// FetchIntradayMetrics returns samples of metric with RecordedAt in r.
	FetchIntradayMetrics(ctx context.Context, conn Connection, metric MetricType, r TimeRange) ([]IntradayPoint, error)
	// FetchSleepSessions returns sessions whose local wake-up date is in r.
	FetchSleepSessions(ctx context.Context, conn Connection, r DateRange) ([]SleepSession, error)
	// RefreshAuth exchanges the refresh token for new credentials.
	RefreshAuth(ctx context.Context, conn Connection) (Credentials, error)
}
