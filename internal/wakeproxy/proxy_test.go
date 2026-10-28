package wakeproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truvity/asg/internal/wakeconfig"
)

const (
	selKey = "role"
	selVal = "builder"
	actTag = "wake-proxy/last-active"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeCloud simulates EC2: a started instance is pending for startPolls
// describe calls, a stopping one for stopPolls, then the state settles.
type fakeCloud struct {
	mu         sync.Mutex
	insts      []*Instance
	pending    map[string]int
	startPolls int
	stopPolls  int
	onRunning  func()
	starts     int
	stops      int
	tags       []string
	neverRun   bool
}

func (f *fakeCloud) Find(_ context.Context, key, value string) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []Instance

	for _, i := range f.insts {
		if i.Tags[key] != value {
			continue
		}

		if n, ok := f.pending[i.ID]; ok && (!f.neverRun || i.State != StatePending) {
			if n <= 0 {
				delete(f.pending, i.ID)

				switch i.State {
				case StatePending:
					i.State = StateRunning
					if f.onRunning != nil {
						f.onRunning()
					}
				case StateStopping:
					i.State = StateStopped
				}
			} else {
				f.pending[i.ID] = n - 1
			}
		}

		out = append(out, *i)
	}

	return out, nil
}

func (f *fakeCloud) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.starts++

	for _, i := range f.insts {
		if i.ID == id && i.State == StateStopped {
			i.State = StatePending
			f.pending[id] = f.startPolls
		}
	}

	return nil
}

func (f *fakeCloud) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stops++

	for _, i := range f.insts {
		if i.ID == id {
			i.State = StateStopping
			f.pending[id] = f.stopPolls
		}
	}

	return nil
}

func (f *fakeCloud) Tag(_ context.Context, id, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, i := range f.insts {
		if i.ID == id {
			i.Tags[key] = value
		}
	}

	f.tags = append(f.tags, value)

	return nil
}

func (f *fakeCloud) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.starts, f.stops
}

func newCloud(state string, n int) *fakeCloud {
	f := &fakeCloud{pending: map[string]int{}}

	for i := range n {
		f.insts = append(f.insts, &Instance{ID: "i-" + strconv.Itoa(i), State: state, PrivateIP: "127.0.0.1", Tags: map[string]string{selKey: selVal}})
	}

	return f
}

// echoBackend listens on a port picked now; open() starts accepting.
type echoBackend struct {
	port int
	mu   sync.Mutex
	l    net.Listener
}

func newBackend(t *testing.T) *echoBackend {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	b := &echoBackend{port: l.Addr().(*net.TCPAddr).Port}
	_ = l.Close()

	t.Cleanup(func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		if b.l != nil {
			_ = b.l.Close()
		}
	})

	return b
}

func (b *echoBackend) open() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.l != nil {
		return
	}

	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(b.port))
	if err != nil {
		return
	}

	b.l = l

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = c.Close() }()

				_, _ = io.Copy(c, c)
			}()
		}
	}()
}

type harness struct {
	p     *Proxy
	cloud *fakeCloud
	clk   *clock
	addr  string
	stop  func()
}

func start(t *testing.T, cloud *fakeCloud, b *echoBackend, mut func(*wakeconfig.Config, *Options)) *harness {
	t.Helper()

	clk := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	cfg := wakeconfig.Config{
		Region:     "eu-west-1",
		HealthPort: 1,
		Routes:     []wakeconfig.Route{{Name: "b", ListenPort: 2, Selector: wakeconfig.Selector{TagKey: selKey, TagValue: selVal}, TargetPort: b.port}},
	}.WithDefaults()
	cfg.WakeTimeout = wakeconfig.Duration(5 * time.Second)
	cfg.ShutdownGrace = wakeconfig.Duration(time.Second)

	opt := Options{
		PollInterval: 5 * time.Millisecond, DialRetry: 5 * time.Millisecond, DialTimeout: time.Second, RestartAfter: time.Hour,
		ActivityInterval: time.Hour, IdleCheckInterval: time.Hour, Now: clk.Now,
	}

	if mut != nil {
		mut(&cfg, &opt)
	}

	p, err := New(cfg, cloud, slog.New(slog.NewTextHandler(io.Discard, nil)), opt)
	if err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = p.Serve(ctx, map[string]net.Listener{"b": l})
	}()

	h := &harness{p: p, cloud: cloud, clk: clk, addr: l.Addr().String(), stop: func() { cancel(); <-done }}
	t.Cleanup(h.stop)

	return h
}

func roundTrip(t *testing.T, addr, msg string) {
	t.Helper()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}

	// Half-close: the echo must still come back.
	_ = c.(*net.TCPConn).CloseWrite()

	got, err := io.ReadAll(c)
	if err != nil || string(got) != msg {
		t.Fatalf("echo: %q, %v; want %q", got, err, msg)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()

	for range 400 {
		if ok() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func TestRunningTargetIsSplicedWithoutAStart(t *testing.T) {
	b := newBackend(t)
	b.open()

	h := start(t, newCloud(StateRunning, 1), b, nil)
	roundTrip(t, h.addr, "hello through the proxy")

	if s, _ := h.cloud.counts(); s != 0 {
		t.Errorf("started a running instance %d times", s)
	}

	waitFor(t, "last-close tag", func() bool { h.cloud.mu.Lock(); defer h.cloud.mu.Unlock(); return len(h.cloud.tags) >= 2 })
}

func TestStoppedTargetIsWokenOnceForConcurrentClients(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopped, 1)
	cloud.startPolls = 6
	cloud.onRunning = func() { time.Sleep(30 * time.Millisecond); b.open() } // running, port not yet open

	h := start(t, cloud, b, nil)

	var wg sync.WaitGroup

	for i := range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			roundTrip(t, h.addr, "client "+strconv.Itoa(i))
		}()
	}

	wg.Wait()

	if s, _ := cloud.counts(); s != 1 {
		t.Errorf("StartInstances called %d times, want 1", s)
	}

	if got := h.p.routes[0].m.wakes.Load(); got != 1 {
		t.Errorf("wakes %d, want 1", got)
	}

	if h.p.routes[0].m.wakeCount != 1 {
		t.Errorf("wake duration observed %d times, want 1", h.p.routes[0].m.wakeCount)
	}
}

func TestWakeTimeoutClosesTheClient(t *testing.T) {
	b := newBackend(t) // never opened
	cloud := newCloud(StateStopped, 1)
	cloud.startPolls = 3
	cloud.onRunning = func() {}

	h := start(t, cloud, b, func(c *wakeconfig.Config, _ *Options) { c.WakeTimeout = wakeconfig.Duration(300 * time.Millisecond) })

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Read(make([]byte, 1)); err == nil || time.Since(begin) < 250*time.Millisecond {
		t.Fatalf("want the connection held for the wake timeout and then closed; err %v after %s", err, time.Since(begin))
	}

	waitFor(t, "wake failure", func() bool { return h.p.routes[0].m.wakeFailures.Load() == 1 })

	if s, _ := cloud.counts(); s != 1 {
		t.Errorf("starts %d, want 1", s)
	}
}

func TestStoppingTargetIsStartedAfterItStopped(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopping, 1)
	cloud.pending["i-0"] = 4
	cloud.startPolls = 1
	cloud.onRunning = b.open

	h := start(t, cloud, b, nil)
	roundTrip(t, h.addr, "after a stop")

	if s, _ := cloud.counts(); s != 1 {
		t.Errorf("starts %d, want 1", s)
	}
}

func TestIdleStopFollowsTheSharedTag(t *testing.T) {
	b := newBackend(t)
	b.open()
	cloud := newCloud(StateRunning, 1)
	h := start(t, cloud, b, nil)
	r := h.p.routes[0]
	ctx := context.Background()

	// No tag: left alone until the idle timeout has passed since the proxy started.
	r.idleCheck(ctx)

	if _, st := cloud.counts(); st != 0 {
		t.Fatal("stopped a target with no tag right after the proxy started")
	}

	h.clk.Advance(19 * time.Minute)
	r.idleCheck(ctx)

	if _, st := cloud.counts(); st != 0 {
		t.Fatal("stopped a target with no tag before the idle timeout")
	}

	// Another proxy used it a minute ago.
	cloud.insts[0].Tags[actTag] = h.clk.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	h.clk.Advance(2 * time.Minute)
	r.idleCheck(ctx)

	if _, st := cloud.counts(); st != 0 {
		t.Fatal("stopped a target whose tag is recent")
	}

	// The tag is now older than the timeout.
	h.clk.Advance(20 * time.Minute)
	r.idleCheck(ctx)

	if _, st := cloud.counts(); st != 1 {
		t.Fatalf("stops %d, want 1", st)
	}

	// Stopping: nothing more to do.
	r.idleCheck(ctx)

	if _, st := cloud.counts(); st != 1 {
		t.Fatalf("stops %d after the target was stopping, want 1", st)
	}

	// A missing tag also stops after the timeout since the proxy started.
	cloud2 := newCloud(StateRunning, 1)
	h2 := start(t, cloud2, b, nil)
	h2.clk.Advance(21 * time.Minute)
	h2.p.routes[0].idleCheck(ctx)

	if _, st := cloud2.counts(); st != 1 {
		t.Fatalf("a tagless target idle past the timeout: stops %d, want 1", st)
	}
}

func TestNoStopWhileConnectionsAreOpen(t *testing.T) {
	b := newBackend(t)
	b.open()
	cloud := newCloud(StateRunning, 1)
	h := start(t, cloud, b, nil)
	r := h.p.routes[0]

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "connection counted", func() bool { return r.activeNow() == 1 && h.p.routes[0].m.connections.Load() == 1 })
	waitFor(t, "established", func() bool {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_, err := c.Write([]byte("x"))

		return err == nil
	})

	// Hours pass with the connection open and the tag stale.
	h.clk.Advance(3 * time.Hour)
	cloud.mu.Lock()
	cloud.insts[0].Tags[actTag] = h.clk.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	cloud.mu.Unlock()

	r.idleCheck(context.Background())

	if _, st := cloud.counts(); st != 0 {
		t.Fatal("stopped a target with an open connection")
	}

	_ = c.Close()

	waitFor(t, "connection closed", func() bool { return r.activeNow() == 0 })
	waitFor(t, "tag refreshed on the last close", func() bool {
		cloud.mu.Lock()
		defer cloud.mu.Unlock()

		return cloud.insts[0].Tags[actTag] == h.clk.Now().UTC().Format(time.RFC3339)
	})

	// The close refreshed the tag, so the target is not idle yet.
	r.idleCheck(context.Background())

	if _, st := cloud.counts(); st != 0 {
		t.Fatal("stopped a target right after its last connection closed")
	}

	h.clk.Advance(21 * time.Minute)
	r.idleCheck(context.Background())

	if _, st := cloud.counts(); st != 1 {
		t.Fatalf("stops %d, want 1", st)
	}
}

func TestZeroOrSeveralMatchingInstancesFailLoudly(t *testing.T) {
	for name, n := range map[string]int{"none": 0, "two": 2} {
		b := newBackend(t)
		b.open()
		cloud := newCloud(StateStopped, n)
		h := start(t, cloud, b, nil)

		c, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}

		_, _ = c.Write([]byte("x"))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Errorf("%s: the connection stayed open", name)
		}

		_ = c.Close()

		if s, _ := cloud.counts(); s != 0 {
			t.Errorf("%s: started an instance", name)
		}

		if h.p.routes[0].m.resolveFailures.Load() == 0 {
			t.Errorf("%s: no resolve failure counted", name)
		}

		_, err = lookup(context.Background(), cloud, selKey, selVal)
		if (n == 0 && err == nil) || (n == 2 && err == nil) {
			t.Errorf("%s: lookup returned no error", name)
		}
	}
}

func TestHealthIgnoresTargetsAndMetricsRender(t *testing.T) {
	b := newBackend(t)
	h := start(t, newCloud(StateStopped, 1), b, nil)

	rec := httptest.NewRecorder()
	h.p.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("health %d with a stopped target, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	wants := []string{`wake_proxy_active_connections{route="b"} 0`, "wake_proxy_wakes_total",
		"wake_proxy_connections_without_data_total", "wake_proxy_stops_total", "wake_proxy_dial_failures_total",
		"wake_proxy_wake_duration_seconds_count"}

	for _, want := range wants {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func (f *fakeCloud) tagCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.tags)
}

func TestProbeConnectAndCloseDoesNotWakeOrTag(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopped, 1)
	h := start(t, cloud, b, nil)

	for range 3 {
		c, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}

		_ = c.Close()
	}

	waitFor(t, "probes seen", func() bool { return h.p.routes[0].m.noData.Load() == 3 })

	if s, _ := cloud.counts(); s != 0 {
		t.Errorf("a probe started the target %d times", s)
	}

	if n := cloud.tagCount(); n != 0 {
		t.Errorf("a probe wrote the activity tag %d times", n)
	}

	if m := h.p.routes[0].m; m.connections.Load() != 0 || h.p.routes[0].activeNow() != 0 {
		t.Errorf("a probe was counted: connections %d, active %d", m.connections.Load(), h.p.routes[0].activeNow())
	}
}

func TestFirstByteWakesTheTargetAndArrivesIntact(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopped, 1)
	cloud.startPolls = 2
	cloud.onRunning = b.open

	h := start(t, cloud, b, nil)

	msg := "\x16\x03\x01 first byte must lead the stream"
	roundTrip(t, h.addr, msg)

	if s, _ := cloud.counts(); s != 1 {
		t.Errorf("starts %d, want 1", s)
	}

	waitFor(t, "byte counts", func() bool { return h.p.routes[0].activeNow() == 0 })
}

func TestWakeOnConnectKeepsTheOldBehaviour(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopped, 1)
	cloud.startPolls = 1
	cloud.onRunning = b.open

	h := start(t, cloud, b, func(c *wakeconfig.Config, _ *Options) { c.Routes[0].WakeOn = wakeconfig.WakeOnConnect })

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "wake without a byte", func() bool { s, _ := cloud.counts(); return s == 1 })
	waitFor(t, "counted", func() bool { return h.p.routes[0].activeNow() == 1 })

	_ = c.Close()

	waitFor(t, "closed", func() bool { return h.p.routes[0].activeNow() == 0 })

	if n := cloud.tagCount(); n < 2 {
		t.Errorf("tag writes %d, want the first-open and last-close writes", n)
	}
}

func TestFirstByteTimeoutClosesWithoutWaking(t *testing.T) {
	b := newBackend(t)
	cloud := newCloud(StateStopped, 1)

	h := start(t, cloud, b, func(c *wakeconfig.Config, _ *Options) {
		c.Routes[0].FirstByteTimeout = wakeconfig.Duration(100 * time.Millisecond)
	})

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("want the proxy to close the silent client; got %v", err)
	}

	if s, _ := cloud.counts(); s != 0 {
		t.Errorf("starts %d, want 0", s)
	}

	if n := cloud.tagCount(); n != 0 {
		t.Errorf("tag writes %d, want 0", n)
	}

	if got := h.p.routes[0].m.noData.Load(); got != 1 {
		t.Errorf("without-data count %d, want 1", got)
	}
}
