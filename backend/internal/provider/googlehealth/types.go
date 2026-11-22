package googlehealth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// Google Health API v4 wire types. Only the fields Pulse maps are declared;
// everything else is ignored. These types never leave this package.

// flexInt decodes an int64 sent either as a JSON string (the API's encoding
// of 64-bit integers) or as a number.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("decoding integer: %w", err)
	}
	*f = flexInt(n)
	return nil
}

type civilDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

func toCivilDate(d domain.Date) civilDate {
	return civilDate{Year: d.Year, Month: int(d.Month), Day: d.Day}
}

// date converts to a domain.Date, reporting whether it is a valid day.
func (c civilDate) date() (domain.Date, bool) {
	d := domain.Date{Year: c.Year, Month: time.Month(c.Month), Day: c.Day}
	return d, d.Validate() == nil
}

type civilTime struct {
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
	Seconds int `json:"seconds"`
	Nanos   int `json:"nanos"`
}

type civilDateTime struct {
	Date civilDate `json:"date"`
	Time civilTime `json:"time"`
}

type sampleTime struct {
	PhysicalTime time.Time `json:"physicalTime"`
}

type interval struct {
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
}

// dataPoint is a list/reconcile result. At most one value field is set.
type dataPoint struct {
	Name          string `json:"name"`
	DataPointName string `json:"dataPointName"` // reconcile responses

	Steps *struct {
		Interval interval `json:"interval"`
		Count    *flexInt `json:"count"` // omitted for worn-but-idle intervals
	} `json:"steps"`
	HeartRate *struct {
		SampleTime     sampleTime `json:"sampleTime"`
		BeatsPerMinute *flexInt   `json:"beatsPerMinute"`
	} `json:"heartRate"`
	HeartRateVariability *struct {
		SampleTime sampleTime `json:"sampleTime"`
		RMSSDMs    *float64   `json:"rootMeanSquareOfSuccessiveDifferencesMilliseconds"`
	} `json:"heartRateVariability"`
	OxygenSaturation *struct {
		SampleTime sampleTime `json:"sampleTime"`
		Percentage *float64   `json:"percentage"`
	} `json:"oxygenSaturation"`
	DailyRestingHeartRate *struct {
		Date           civilDate `json:"date"`
		BeatsPerMinute *flexInt  `json:"beatsPerMinute"`
	} `json:"dailyRestingHeartRate"`
	DailyHeartRateVariability *struct {
		Date      civilDate `json:"date"`
		AverageMs *float64  `json:"averageHeartRateVariabilityMilliseconds"`
	} `json:"dailyHeartRateVariability"`
	DailyOxygenSaturation *struct {
		Date              civilDate `json:"date"`
		AveragePercentage *float64  `json:"averagePercentage"`
	} `json:"dailyOxygenSaturation"`
	Sleep *sleepValue `json:"sleep"`
}

type sleepValue struct {
	Interval interval `json:"interval"`
	Type     string   `json:"type"` // CLASSIC | STAGES
	Stages   []struct {
		StartTime time.Time `json:"startTime"`
		EndTime   time.Time `json:"endTime"`
		Type      string    `json:"type"`
	} `json:"stages"`
	Metadata struct {
		MainSleep bool `json:"mainSleep"`
	} `json:"metadata"`
	Summary struct {
		MinutesAsleep        *flexInt `json:"minutesAsleep"`
		MinutesInSleepPeriod *flexInt `json:"minutesInSleepPeriod"`
	} `json:"summary"`
}

type pageResponse struct {
	DataPoints    []dataPoint `json:"dataPoints"`
	NextPageToken string      `json:"nextPageToken"`
}

// rollupPoint is a rollUp (physical windows) or dailyRollUp (civil days) result.
type rollupPoint struct {
	StartTime      time.Time      `json:"startTime"`      // rollUp
	CivilStartTime *civilDateTime `json:"civilStartTime"` // dailyRollUp
	Steps          *struct {
		CountSum *flexInt `json:"countSum"`
	} `json:"steps"`
	TotalCalories *struct {
		KcalSum *float64 `json:"kcalSum"`
	} `json:"totalCalories"`
}

type rollupResponse struct {
	RollupDataPoints []rollupPoint `json:"rollupDataPoints"`
	NextPageToken    string        `json:"nextPageToken"`
}

type dailyRollUpRequest struct {
	Range struct {
		Start civilDateTime `json:"start"`
		End   civilDateTime `json:"end"`
	} `json:"range"`
	WindowSizeDays int    `json:"windowSizeDays"`
	PageSize       int    `json:"pageSize"`
	PageToken      string `json:"pageToken,omitempty"`
}

type rollUpRequest struct {
	Range struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
	} `json:"range"`
	WindowSize string `json:"windowSize"`
	PageSize   int    `json:"pageSize"`
	PageToken  string `json:"pageToken,omitempty"`
}

// tokenResponse is the OAuth token endpoint's success body. It holds secrets:
// never log it or put it in an error.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// tokenError is the OAuth token endpoint's error body (RFC 6749 §5.2).
type tokenError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// rpcError is a google.rpc.Status error body (AIP-193).
type rpcError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

var _ json.Unmarshaler = (*flexInt)(nil)
