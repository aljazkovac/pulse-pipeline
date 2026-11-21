package store

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// Conversions between domain values and pgtype nullable wrappers.

func pgDate(d domain.Date) pgtype.Date { return pgtype.Date{Time: d.Time(), Valid: true} }

func fromPgDate(d pgtype.Date) domain.Date { return domain.DateOf(d.Time) }

func pgInt2(v *int16) pgtype.Int2 {
	if v == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: *v, Valid: true}
}

func fromInt2(v pgtype.Int2) *int16 {
	if !v.Valid {
		return nil
	}
	return &v.Int16
}

func pgInt4(v *int32) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *v, Valid: true}
}

func fromInt4(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

func pgFloat8(v *float64) pgtype.Float8 {
	if v == nil {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *v, Valid: true}
}

func fromFloat8(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
