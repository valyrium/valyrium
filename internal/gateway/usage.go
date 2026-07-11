package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
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

// usageFile is the on-disk form: one entry per local calendar day, keyed by
// the date in usageDayLayout.
type usageFile struct {
	Days map[string]DayUsage `json:"days"`
}

// UsageStore accumulates token/cost usage per local calendar day so
// week/month/YTD/all-time totals survive a restart. The whole ledger is one
// entry per day — a few hundred per year — so it is held in memory and
// rewritten in full on each record; there is no benefit to a keyed database
// at that size (ADR 0004).
//
// A disabled store (path "off", or a file that could not be opened) has a nil
// days map and every method is a no-op: usage tracking is analytics and must
// never take down the gateway.
type UsageStore struct {
	mu   sync.Mutex
	path string
	days map[string]DayUsage
}

// OpenUsageStore opens the usage ledger at path. The literal string "off"
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

	days, err := loadUsageFile(path)
	if err != nil {
		log.Printf("warning: usage tracking disabled: %v", err)
		return &UsageStore{}
	}

	store := &UsageStore{path: path, days: days}
	// Write the ledger back out once up front: a store that cannot persist is
	// worse than no store at all, so find that out at startup rather than on
	// the first completed request.
	if err := store.save(); err != nil {
		log.Printf("warning: usage tracking disabled: %v", err)
		return &UsageStore{}
	}
	return store
}

// loadUsageFile reads the ledger. A missing file is an empty ledger, not an
// error — that is simply the first run.
func loadUsageFile(path string) (map[string]DayUsage, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]DayUsage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]DayUsage{}, nil
	}

	var file usageFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("usage file %s is not readable: %w", path, err)
	}
	if file.Days == nil {
		file.Days = map[string]DayUsage{}
	}
	return file.Days, nil
}

// save writes the whole ledger to a temp file in the same directory and
// renames it over the real one. The rename is atomic, so a crash mid-write
// leaves the previous ledger intact rather than a half-written file. Callers
// hold u.mu.
func (u *UsageStore) save() error {
	encoded, err := json.Marshal(usageFile{Days: u.days})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(u.path), filepath.Base(u.path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename succeeds

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), u.path)
}

func defaultUsageDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "valyrium-usage.db"
	}
	return filepath.Join(home, ".valyrium", "usage.db")
}

func (u *UsageStore) enabled() bool {
	return u != nil && u.days != nil
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

	u.mu.Lock()
	defer u.mu.Unlock()

	key := day.Format(usageDayLayout)
	total := u.days[key]
	total.InputTokens += int64(inputTokens)
	total.OutputTokens += int64(outputTokens)
	total.CostUSD += cost
	u.days[key] = total

	if err := u.save(); err != nil {
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

	u.mu.Lock()
	defer u.mu.Unlock()

	for key, usage := range u.days {
		day, err := time.ParseInLocation(usageDayLayout, key, now.Location())
		if err != nil {
			continue // not a date key; ignore rather than fail the scrape
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

// Close flushes the ledger. Every record already writes through to disk, so
// this only guards against a partial in-memory state going unwritten.
func (u *UsageStore) Close() error {
	if !u.enabled() {
		return nil
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	return u.save()
}
