package gateway

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newUsageStore(t *testing.T) *UsageStore {
	t.Helper()

	store := OpenUsageStore(filepath.Join(t.TempDir(), "usage.db"))
	if !store.enabled() {
		t.Fatal("expected an enabled usage store")
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func costOf(v float64) *float64 { return &v }

func TestUsageStoreRecordAndAggregate(t *testing.T) {
	store := newUsageStore(t)

	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.Local) // a Wednesday
	day := func(d int) time.Time {
		return time.Date(2026, time.July, d, 9, 0, 0, 0, time.Local)
	}

	// Two records on the same day must accumulate, not overwrite.
	store.recordUsageOn(day(15), 100, 10, costOf(0.5))
	store.recordUsageOn(day(15), 50, 5, costOf(0.25))
	store.recordUsageOn(day(14), 200, 20, costOf(1.0)) // earlier this week
	store.recordUsageOn(day(1), 400, 40, costOf(2.0))  // earlier this month
	store.recordUsageOn(time.Date(2026, time.March, 3, 9, 0, 0, 0, time.Local), 800, 80, costOf(4.0))
	store.recordUsageOn(time.Date(2025, time.December, 31, 9, 0, 0, 0, time.Local), 1600, 160, costOf(8.0))

	// A nil cost must count as zero rather than erroring or being guessed.
	store.recordUsageOn(day(15), 7, 3, nil)

	totals := store.Aggregate(now)

	want := map[string]DayUsage{
		"current": {InputTokens: 157, OutputTokens: 18, CostUSD: 0.75},
		"week":    {InputTokens: 357, OutputTokens: 38, CostUSD: 1.75},
		"month":   {InputTokens: 757, OutputTokens: 78, CostUSD: 3.75},
		"ytd":     {InputTokens: 1557, OutputTokens: 158, CostUSD: 7.75},
		"all":     {InputTokens: 3157, OutputTokens: 318, CostUSD: 15.75},
	}
	for _, period := range usagePeriods {
		got := totals[period]
		if got.InputTokens != want[period].InputTokens || got.OutputTokens != want[period].OutputTokens {
			t.Errorf("period %q: got %d in / %d out, want %d in / %d out",
				period, got.InputTokens, got.OutputTokens, want[period].InputTokens, want[period].OutputTokens)
		}
		if diff := got.CostUSD - want[period].CostUSD; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("period %q: got cost %v, want %v", period, got.CostUSD, want[period].CostUSD)
		}
	}

	var out bytes.Buffer
	store.WritePrometheus(&out)
	if !strings.Contains(out.String(), `llmgateway_usage_input_tokens{period="all"} 3157`) {
		t.Errorf("expected the all-time input gauge in:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `llmgateway_usage_cost_usd{period="all"} 15.75`) {
		t.Errorf("expected the all-time cost gauge in:\n%s", out.String())
	}
}

func TestUsageStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.Local)
	day := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.Local)

	first := OpenUsageStore(path)
	if !first.enabled() {
		t.Fatal("expected an enabled usage store")
	}
	first.recordUsageOn(day, 100, 10, costOf(0.5))
	first.recordUsageOn(day.AddDate(0, 0, -1), 200, 20, costOf(1.0))
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A second process against the same file must see the first one's totals —
	// that is the whole point of persisting.
	second := OpenUsageStore(path)
	if !second.enabled() {
		t.Fatal("expected an enabled usage store after reopening")
	}
	t.Cleanup(func() { _ = second.Close() })

	all := second.Aggregate(now)["all"]
	if all.InputTokens != 300 || all.OutputTokens != 30 {
		t.Errorf("after restart: %d in / %d out, want 300 in / 30 out", all.InputTokens, all.OutputTokens)
	}
	if diff := all.CostUSD - 1.5; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("after restart: cost %v, want 1.5", all.CostUSD)
	}

	// Recording against the reopened store must add to the reloaded totals
	// rather than start over from an empty ledger.
	second.recordUsageOn(day, 1, 1, nil)
	if got := second.Aggregate(now)["all"].InputTokens; got != 301 {
		t.Errorf("after restart and one more record: %d in, want 301", got)
	}
}

func TestUsagePeriodBoundaries(t *testing.T) {
	// A Wednesday: its ISO week began Monday 2026-07-13.
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.Local)
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	}

	tests := []struct {
		name string
		day  time.Time
		want []string
	}{
		{"today", day(2026, time.July, 15), []string{"all", "current", "month", "week", "ytd"}},
		{"monday of this week", day(2026, time.July, 13), []string{"all", "month", "week", "ytd"}},
		{"sunday before this week", day(2026, time.July, 12), []string{"all", "month", "ytd"}},
		{"eight days ago", day(2026, time.July, 7), []string{"all", "month", "ytd"}},
		{"first of this month", day(2026, time.July, 1), []string{"all", "month", "ytd"}},
		{"last day of last month", day(2026, time.June, 30), []string{"all", "ytd"}},
		{"january first", day(2026, time.January, 1), []string{"all", "ytd"}},
		{"new year's eve last year", day(2025, time.December, 31), []string{"all"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := periodsIn(periodsFor(tc.day, now))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("%s counts toward %v, want %v", tc.day.Format(usageDayLayout), got, tc.want)
			}
		})
	}

	// On a Sunday, the ISO week still starts on the preceding Monday rather
	// than that same day — the case Go's Sunday-is-0 weekday numbering gets
	// wrong if it is not corrected.
	sunday := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.Local)
	if !periodsFor(day(2026, time.July, 6), sunday)["week"] {
		t.Error("Monday 2026-07-06 should be in the week of Sunday 2026-07-12")
	}
	if periodsFor(day(2026, time.July, 5), sunday)["week"] {
		t.Error("Sunday 2026-07-05 should not be in the week of Sunday 2026-07-12")
	}
}

func periodsIn(in map[string]bool) []string {
	var out []string
	for period, counts := range in {
		if counts {
			out = append(out, period)
		}
	}
	sort.Strings(out)
	return out
}

func TestUsageStoreDisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // guard: a bug that ignores "off" must not fall back to $HOME

	store := OpenUsageStore("off")
	if store.enabled() {
		t.Fatal(`OpenUsageStore("off") returned an enabled store`)
	}

	store.RecordUsage(100, 10, costOf(1.0))
	store.recordUsageOn(time.Now(), 100, 10, nil)

	if totals := store.Aggregate(time.Now()); len(totals) != 0 {
		t.Errorf("disabled store aggregated %v, want nothing", totals)
	}

	// Gauges are omitted entirely, not written as zero, so a scraper can tell
	// "tracking off" from "genuinely zero usage".
	var out bytes.Buffer
	store.WritePrometheus(&out)
	if out.Len() != 0 {
		t.Errorf("disabled store wrote metrics:\n%s", out.String())
	}
	if err := store.Close(); err != nil {
		t.Errorf("Close on a disabled store: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("disabled store created files: %v", entries)
	}
}

func TestUsageStoreOpenFailureNonFatal(t *testing.T) {
	// A regular file where the store wants a directory: MkdirAll cannot
	// create the parent, so the open fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	unopenable := filepath.Join(blocker, "usage.db")

	store := OpenUsageStore(unopenable)
	if store.enabled() {
		t.Fatal("expected a disabled store after a failed open")
	}
	store.RecordUsage(100, 10, costOf(1.0)) // must not panic

	// The gateway itself must still start and serve: usage tracking is
	// analytics and never takes down the primary function (ADR 0004).
	server := NewServer(Config{
		Host:         "127.0.0.1",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet"},
		Concurrency:  4,
		UsageDB:      unopenable,
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /healthz with an unopenable usage db: expected status 200, got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /metrics with an unopenable usage db: expected status 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "llmgateway_usage_") {
		t.Errorf("expected no usage gauges when tracking is off:\n%s", w.Body.String())
	}
}

func TestChatCompletionsRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	stubBin := filepath.Join(dir, "claude-stub")
	stubScript := `#!/bin/sh
echo '{"type":"result","result":"hello","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}'
exit 0
`
	if err := os.WriteFile(stubBin, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	server := NewServer(Config{
		Host:         "127.0.0.1",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet"},
		ClaudeBin:    stubBin,
		TimeoutMS:    30000,
		Concurrency:  4,
		UsageDB:      filepath.Join(dir, "usage.db"),
	})
	t.Cleanup(func() { _ = server.metrics.usage.Close() })

	body := `{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST /v1/chat/completions: expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Prompt tokens are input + cache reads + cache creation, matching what
	// the response and the request log already report.
	totals := server.metrics.usage.Aggregate(time.Now())
	all := totals["all"]
	if all.InputTokens != 13 || all.OutputTokens != 2 {
		t.Errorf("recorded %d in / %d out, want 13 in / 2 out", all.InputTokens, all.OutputTokens)
	}
	if diff := all.CostUSD - 0.001; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("recorded cost %v, want 0.001", all.CostUSD)
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, req)
	for _, want := range []string{
		`llmgateway_usage_input_tokens{period="current"} 13`,
		`llmgateway_usage_output_tokens{period="current"} 2`,
		`llmgateway_usage_cost_usd{period="all"} 0.001`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q in /metrics:\n%s", want, w.Body.String())
		}
	}
}
