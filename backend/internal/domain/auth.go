package domain

import (
	"time"

	"github.com/google/uuid"
)

// Identity is an OpenID Connect identity whose ID token has been validated
// (ADR-0005 §2). (Issuer, Subject) identifies the account; Email is the
// address the identity provider reported at the time.
type Identity struct {
	Issuer  string
	Subject string
	Email   string
}

// Session is a server-side sign-in session (ADR-0005 §3). It never holds the
// session secret: the store keys sessions by the secret's SHA-256.
type Session struct {
	UserID     uuid.UUID
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute expiry
}
