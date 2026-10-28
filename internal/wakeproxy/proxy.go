package wakeproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/truvity/asg/internal/wakeconfig"
)

// Options are the timings that are not part of the config file. The zero
// value of each field is its default; tests shorten them.
type Options struct {
	// PollInterval between DescribeInstances calls while a target changes
	// state. Default 2s.
	PollInterval time.Duration
	// DialTimeout of one connection attempt to the target. Default 3s.
	DialTimeout time.Duration
	// DialRetry between attempts while the port does not accept. Default 1s.
	DialRetry time.Duration
	// RestartAfter is how long a started instance may still read as stopped
	// before the start is issued again. Default 30s.
	RestartAfter time.Duration
	// ActivityInterval is the most often the activity tag is refreshed
	// while connections are open. Default 1m.
	ActivityInterval time.Duration
	// IdleCheckInterval between checks for an idle target. Default 1m.
	IdleCheckInterval time.Duration
	// APITimeout bounds one start, stop or tag call. Default 30s.
	APITimeout time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	def := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}

	def(&o.PollInterval, 2*time.Second)
	def(&o.DialTimeout, 3*time.Second)
	def(&o.DialRetry, time.Second)
	def(&o.RestartAfter, 30*time.Second)
	def(&o.ActivityInterval, time.Minute)
	def(&o.IdleCheckInterval, time.Minute)
	def(&o.APITimeout, 30*time.Second)

	if o.Now == nil {
		o.Now = time.Now
	}

	return o
}

// Proxy serves the routes of a config.
type Proxy struct {
	cfg     wakeconfig.Config
	cloud   Cloud
	log     *slog.Logger
	opt     Options
	routes  []*route
	started time.Time

	// connCtx lives until the shutdown grace has passed. It is set before
	// any goroutine of Serve starts.
	connCtx context.Context

	conns   sync.WaitGroup
	trackMu sync.Mutex
	tracked map[net.Conn]struct{}
}

// New returns a proxy. cfg must have had its defaults applied and be valid.
func New(cfg wakeconfig.Config, cloud Cloud, log *slog.Logger, opt Options) (*Proxy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opt = opt.withDefaults()
	p := &Proxy{cfg: cfg, cloud: cloud, log: log, opt: opt, started: opt.Now(), tracked: map[net.Conn]struct{}{}}

	for _, rc := range cfg.Routes {
		p.routes = append(p.routes, &route{p: p, cfg: rc, log: log.With("route", rc.Name), m: &routeMetrics{}})
	}

	return p, nil
}

func (p *Proxy) track(c net.Conn) {
	p.trackMu.Lock()
	p.tracked[c] = struct{}{}
	p.trackMu.Unlock()
}

func (p *Proxy) untrack(c net.Conn) {
	p.trackMu.Lock()
	delete(p.tracked, c)
	p.trackMu.Unlock()
}

func (p *Proxy) closeAll() {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()

	for c := range p.tracked {
		_ = c.Close()
	}
}

// HealthHandler answers 200 whenever the proxy runs. It does not look at the
// targets: a target that sleeps is the normal state, and a health check that
// followed it would take the proxy out of its load balancer.
func (p *Proxy) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	return mux
}

// MetricsHandler serves the Prometheus text format.
func (p *Proxy) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		m := make(map[string]*routeMetrics, len(p.routes))
		for _, r := range p.routes {
			m[r.cfg.Name] = r.m
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, m)
	})

	return mux
}

// Run listens on the configured ports and serves until ctx is cancelled.
func (p *Proxy) Run(ctx context.Context) error {
	var lc net.ListenConfig

	ls := map[string]net.Listener{}

	closeAll := func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}

	for _, r := range p.cfg.Routes {
		l, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(r.ListenPort))
		if err != nil {
			closeAll()

			return fmt.Errorf("route %s: %w", r.Name, err)
		}

		ls[r.Name] = l
	}

	servers := []*http.Server{{Addr: ":" + strconv.Itoa(p.cfg.HealthPort), Handler: p.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}}
	if p.cfg.MetricsAddress != "" {
		servers = append(servers, &http.Server{Addr: p.cfg.MetricsAddress, Handler: p.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second})
	}

	for _, s := range servers {
		l, err := lc.Listen(ctx, "tcp", s.Addr)
		if err != nil {
			closeAll()

			return fmt.Errorf("listen %s: %w", s.Addr, err)
		}

		go func() {
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.log.Error("http server", "addr", s.Addr, "error", err)
			}
		}()
	}

	err := p.Serve(ctx, ls)

	for _, s := range servers {
		_ = s.Close()
	}

	return err
}

// Serve accepts on the listeners (one per route name) until ctx is
// cancelled, then stops accepting and lets the open streams finish for the
// shutdown grace before it closes them. It closes the listeners.
func (p *Proxy) Serve(ctx context.Context, ls map[string]net.Listener) error {
	for _, r := range p.routes {
		if ls[r.cfg.Name] == nil {
			return fmt.Errorf("route %s: no listener", r.cfg.Name)
		}
	}

	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	p.connCtx = connCtx

	var accepting sync.WaitGroup

	for _, r := range p.routes {
		l := ls[r.cfg.Name]

		p.log.Info("serving", "route", r.cfg.Name, "listen", l.Addr().String(), "selector", r.cfg.Selector.TagKey+"="+r.cfg.Selector.TagValue,
			"targetPort", r.cfg.TargetPort, "idleTimeout", p.cfg.IdleTimeout.D().String(), "wakeTimeout", p.cfg.WakeTimeout.D().String())

		accepting.Add(1)

		go func() {
			defer accepting.Done()

			r.acceptLoop(l)
		}()

		go r.idleLoop(ctx)
		go r.activityLoop(connCtx)
		go r.startupCheck(ctx)
	}

	<-ctx.Done()
	p.log.Info("shutting down: no new connections", "grace", p.cfg.ShutdownGrace.D().String())

	for _, r := range p.routes {
		_ = ls[r.cfg.Name].Close()
	}

	accepting.Wait()

	drained := make(chan struct{})

	go func() {
		p.conns.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(p.cfg.ShutdownGrace.D()):
		p.log.Warn("shutdown grace over: closing open connections")
		p.closeAll()
		<-drained
	}

	return nil
}
