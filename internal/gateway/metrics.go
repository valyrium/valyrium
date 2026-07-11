package gateway

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	requestsTotal   map[string]map[int]int64
	inflightGauge   int64
	requestDuration []int64
	mu              sync.RWMutex
}

func NewMetrics() *Metrics {
	return &Metrics{
		requestsTotal:   make(map[string]map[int]int64),
		requestDuration: make([]int64, 0),
	}
}

func (m *Metrics) RecordRequest(method, path string, status int, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s %s", method, path)
	if _, ok := m.requestsTotal[key]; !ok {
		m.requestsTotal[key] = make(map[int]int64)
	}
	m.requestsTotal[key][status]++

	m.requestDuration = append(m.requestDuration, duration.Milliseconds())
}

func (m *Metrics) IncInflight() {
	atomic.AddInt64(&m.inflightGauge, 1)
}

func (m *Metrics) DecInflight() {
	atomic.AddInt64(&m.inflightGauge, -1)
}

func (m *Metrics) WritePrometheus(w io.Writer) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, _ = fmt.Fprintf(w, "# HELP llmgateway_requests_total Total number of requests\n")
	_, _ = fmt.Fprintf(w, "# TYPE llmgateway_requests_total counter\n")

	for key, statuses := range m.requestsTotal {
		for status, count := range statuses {
			_, _ = fmt.Fprintf(w, "llmgateway_requests_total{path=\"%s\",status=\"%d\"} %d\n", key, status, count)
		}
	}

	_, _ = fmt.Fprintf(w, "# HELP llmgateway_inflight_requests Number of in-flight requests\n")
	_, _ = fmt.Fprintf(w, "# TYPE llmgateway_inflight_requests gauge\n")
	_, _ = fmt.Fprintf(w, "llmgateway_inflight_requests %d\n", atomic.LoadInt64(&m.inflightGauge))

	_, _ = fmt.Fprintf(w, "# HELP llmgateway_request_duration_seconds Request duration in seconds\n")
	_, _ = fmt.Fprintf(w, "# TYPE llmgateway_request_duration_seconds summary\n")

	if len(m.requestDuration) > 0 {
		sum := int64(0)
		for _, d := range m.requestDuration {
			sum += d
		}
		_, _ = fmt.Fprintf(w, "llmgateway_request_duration_seconds_sum %f\n", float64(sum)/1000.0)
		_, _ = fmt.Fprintf(w, "llmgateway_request_duration_seconds_count %d\n", len(m.requestDuration))
	} else {
		_, _ = fmt.Fprintf(w, "llmgateway_request_duration_seconds_sum 0\n")
		_, _ = fmt.Fprintf(w, "llmgateway_request_duration_seconds_count 0\n")
	}
}
