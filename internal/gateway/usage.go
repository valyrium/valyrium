package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	usageBucket    = "usage_days"
	usageDayLayout = "2006-01-02"
	usageOff       = "off"
)

// usagePeriods are the cumulative windows the dashboard's token ledger tabs
// use, in display order. A day counts toward every window it falls inside,
// not just the narrowest one.
var usagePeriods = []string{"current", "week", "month", "ytd", "all"}

// DayUsage is the value stored under each calendar-date key.
type DayUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// UsageStore accumulates token/cost usage per local calendar day in a bbolt
// file so week/month/YTD/all-time totals survive a restart. A nil db means
// tracking is disabled — either explicitly, or because the file could not be
// opened. Every method is a no-op in that state: usage tracking is analytics
// and must never take down the gateway (ADR 0004).
type UsageStore struct {
	db *bolt.DB
}

// OpenUsageStore opens the usage database at path. The literal string "off"
// disables tracking; an empty path means the default location. A failure to
// open is logged and degrades to a disabled store, never an error.
func OpenUsageStore(path string) *UsageStore {
	if path == "" {
		path = defaultUsageDBPath()
	}
	if path == usageOff {
		return &UsageStore{}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		log.Printf("warning: usage tracking disabled: %v", err)
		return &UsageStore{}
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Printf("warning: usage tracking disabled: %v", err)
		return &UsageStore{}
	}
	return &UsageStore{db: db}
}

func defaultUsageDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "valyrium-usage.db"
	}
	return filepath.Join(home, ".valyrium", "usage.db")
}

func (u *UsageStore) enabled() bool {
	return u != nil && u.db != nil
}

// RecordUsage adds one request's tokens and cost to today's running totals.
// A nil costUSD counts as zero — the CLI does not always report cost, and an
// undercounted total beats a guessed one.
func (u *UsageStore) RecordUsage(inputTokens, outputTokens int, costUSD *float64) {
	u.recordUsageOn(time.Now(), inputTokens, outputTokens, costUSD)
}

// recordUsageOn is RecordUsage with an explicit clock, so tests can lay down
// usage on specific calendar days.
func (u *UsageStore) recordUsageOn(day time.Time, inputTokens, outputTokens int, costUSD *float64) {
	if !u.enabled() {
		return
	}

	cost := 0.0
	if costUSD != nil {
		cost = *costUSD
	}

	err := u.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(usageBucket))
		if err != nil {
			return err
		}

		key := []byte(day.Format(usageDayLayout))
		var total DayUsage
		if existing := bucket.Get(key); existing != nil {
			if err := json.Unmarshal(existing, &total); err != nil {
				return err
			}
		}

		total.InputTokens += int64(inputTokens)
		total.OutputTokens += int64(outputTokens)
		total.CostUSD += cost

		encoded, err := json.Marshal(total)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
	if err != nil {
		log.Printf("warning: failed to record usage: %v", err)
	}
}

// periodsFor reports which cumulative windows a stored day falls inside,
// relative to now. It takes now explicitly rather than calling time.Now() so
// boundary behaviour is testable against fixed dates.
func periodsFor(day, now time.Time) map[string]bool {
	in := map[string]bool{"all": true}

	year, month, dayOfMonth := now.Date()
	loc := now.Location()
	today := time.Date(year, month, dayOfMonth, 0, 0, 0, 0, loc)

	// ISO weeks start on Monday; Go numbers Sunday as 0.
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	monday := today.AddDate(0, 0, -(weekday - 1))
	firstOfMonth := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	firstOfYear := time.Date(year, time.January, 1, 0, 0, 0, 0, loc)

	in["current"] = day.Equal(today)
	in["week"] = !day.Before(monday)
	in["month"] = !day.Before(firstOfMonth)
	in["ytd"] = !day.Before(firstOfYear)
	return in
}

// Aggregate sums every stored day into the five cumulative periods, relative
// to now. A disabled store aggregates to nothing.
func (u *UsageStore) Aggregate(now time.Time) map[string]DayUsage {
	totals := make(map[string]DayUsage, len(usagePeriods))
	if !u.enabled() {
		return totals
	}

	err := u.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(usageBucket))
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(key, value []byte) error {
			day, err := time.ParseInLocation(usageDayLayout, string(key), now.Location())
			if err != nil {
				return nil // not a date key; ignore rather than fail the scrape
			}
			var usage DayUsage
			if err := json.Unmarshal(value, &usage); err != nil {
				return nil
			}

			for period, counts := range periodsFor(day, now) {
				if !counts {
					continue
				}
				total := totals[period]
				total.InputTokens += usage.InputTokens
				total.OutputTokens += usage.OutputTokens
				total.CostUSD += usage.CostUSD
				totals[period] = total
			}
			return nil
		})
	})
	if err != nil {
		log.Printf("warning: failed to read usage: %v", err)
	}

	return totals
}

// WritePrometheus emits the token-ledger gauges. A disabled store writes
// nothing at all — absent gauges let a scraper distinguish "tracking off"
// from "genuinely zero usage".
func (u *UsageStore) WritePrometheus(w io.Writer) {
	if !u.enabled() {
		return
	}

	totals := u.Aggregate(time.Now())

	line(w, "# HELP llmgateway_usage_input_tokens Cumulative input tokens by period\n")
	line(w, "# TYPE llmgateway_usage_input_tokens gauge\n")
	for _, period := range usagePeriods {
		line(w, "llmgateway_usage_input_tokens{period=%q} %d\n", period, totals[period].InputTokens)
	}

	line(w, "# HELP llmgateway_usage_output_tokens Cumulative output tokens by period\n")
	line(w, "# TYPE llmgateway_usage_output_tokens gauge\n")
	for _, period := range usagePeriods {
		line(w, "llmgateway_usage_output_tokens{period=%q} %d\n", period, totals[period].OutputTokens)
	}

	line(w, "# HELP llmgateway_usage_cost_usd Cumulative cost in USD by period\n")
	line(w, "# TYPE llmgateway_usage_cost_usd gauge\n")
	for _, period := range usagePeriods {
		cost := strconv.FormatFloat(totals[period].CostUSD, 'f', -1, 64)
		line(w, "llmgateway_usage_cost_usd{period=%q} %s\n", period, cost)
	}
}

// A scrape that hangs up mid-write leaves nothing worth doing about it.
func line(w io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func (u *UsageStore) Close() error {
	if !u.enabled() {
		return nil
	}
	return u.db.Close()
}
