package wakeproxy

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// routeMetrics are the counters of one route.
type routeMetrics struct {
	active          atomic.Int64
	connections     atomic.Int64
	noData          atomic.Int64
	wakes           atomic.Int64
	wakeFailures    atomic.Int64
	stops           atomic.Int64
	dialFailures    atomic.Int64
	resolveFailures atomic.Int64

	mu          sync.Mutex
	wakeSeconds float64
	wakeCount   int64
}

func (m *routeMetrics) observeWake(d time.Duration) {
	m.mu.Lock()
	m.wakeSeconds += d.Seconds()
	m.wakeCount++
	m.mu.Unlock()
}

// writeMetrics writes every route's counters in the Prometheus text format.
func writeMetrics(w io.Writer, routes map[string]*routeMetrics) {
	names := make([]string, 0, len(routes))
	for n := range routes {
		names = append(names, n)
	}

	sort.Strings(names)

	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	series := []struct {
		name, kind, help string
		value            func(*routeMetrics) int64
	}{
		{"wake_proxy_active_connections", "gauge", "Connections open now.", func(m *routeMetrics) int64 { return m.active.Load() }},
		{"wake_proxy_connections_total", "counter", "Connections accepted.", func(m *routeMetrics) int64 { return m.connections.Load() }},
		{"wake_proxy_connections_without_data_total", "counter", "Connections that ended before a first byte (probes); not activity.",
			func(m *routeMetrics) int64 { return m.noData.Load() }},
		{"wake_proxy_wakes_total", "counter", "Times the proxy started a stopped target.", func(m *routeMetrics) int64 { return m.wakes.Load() }},
		{"wake_proxy_wake_failures_total", "counter", "Wakes that ended without a reachable target.", func(m *routeMetrics) int64 { return m.wakeFailures.Load() }},
		{"wake_proxy_stops_total", "counter", "Times the proxy stopped an idle target.", func(m *routeMetrics) int64 { return m.stops.Load() }},
		{"wake_proxy_dial_failures_total", "counter", "Failed dials of a target that was reported running.",
			func(m *routeMetrics) int64 { return m.dialFailures.Load() }},
		{"wake_proxy_resolve_failures_total", "counter", "Lookups that matched zero or several instances, or failed.",
			func(m *routeMetrics) int64 { return m.resolveFailures.Load() }},
	}

	for _, s := range series {
		pf("# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.kind)

		for _, n := range names {
			pf("%s{route=%q} %d\n", s.name, n, s.value(routes[n]))
		}
	}

	pf("# HELP wake_proxy_wake_duration_seconds Time from the start request to an accepting port.\n# TYPE wake_proxy_wake_duration_seconds summary\n")

	for _, n := range names {
		m := routes[n]
		m.mu.Lock()
		sum, count := m.wakeSeconds, m.wakeCount
		m.mu.Unlock()

		pf("wake_proxy_wake_duration_seconds_sum{route=%q} %g\nwake_proxy_wake_duration_seconds_count{route=%q} %d\n", n, sum, n, count)
	}
}
