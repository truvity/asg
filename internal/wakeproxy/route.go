package wakeproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/truvity/asg/internal/wakeconfig"
)

// flight is one wake that every waiting client shares.
type flight struct {
	done chan struct{}
	addr string
	err  error
}

type route struct {
	p   *Proxy
	cfg wakeconfig.Route
	log *slog.Logger
	m   *routeMetrics

	// mu guards the fields below, and is held while a target is stopped, so
	// that no connection is counted in between the decision and the stop.
	mu        sync.Mutex
	active    int
	wake      *flight
	lastTouch time.Time
	lastID    string
}

func (r *route) now() time.Time { return r.p.opt.Now() }

func (r *route) resolve(ctx context.Context) (Instance, error) {
	inst, err := lookup(ctx, r.p.cloud, r.cfg.Selector.TagKey, r.cfg.Selector.TagValue)
	if err != nil {
		r.m.resolveFailures.Add(1)

		return Instance{}, err
	}

	r.mu.Lock()
	r.lastID = inst.ID
	r.mu.Unlock()

	return inst, nil
}

func (r *route) addr(inst Instance) string {
	return net.JoinHostPort(inst.PrivateIP, strconv.Itoa(r.cfg.TargetPort))
}

// sleep waits d or until ctx ends. It reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (r *route) startupCheck(ctx context.Context) {
	if inst, err := r.resolve(ctx); err != nil {
		r.log.Error("target selector does not resolve to one instance", "error", err)
	} else {
		r.log.Info("target resolved", "instance", inst.ID, "state", inst.State)
	}
}

func (r *route) acceptLoop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			r.log.Warn("accept", "error", err)

			time.Sleep(50 * time.Millisecond)

			continue
		}

		r.p.conns.Add(1)
		r.p.track(c)

		go func() {
			defer r.p.conns.Done()
			defer r.p.untrack(c)

			r.handle(r.p.connCtx, c)
		}()
	}
}

// open counts a connection and reports whether it is the first.
func (r *route) open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.active++
	r.m.active.Store(int64(r.active))
	r.m.connections.Add(1)

	return r.active == 1
}

// closed uncounts a connection and reports whether it was the last.
func (r *route) closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.active--
	r.m.active.Store(int64(r.active))

	return r.active == 0
}

func (r *route) activeNow() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.active
}

// awaitData waits for the client's first byte, for at most the route's first
// byte timeout. It returns a connection that replays the byte, and false when
// the client closed or the time ran out without one.
func (r *route) awaitData(c net.Conn) (net.Conn, bool) {
	_ = c.SetReadDeadline(time.Now().Add(r.cfg.FirstByteTimeout.D()))

	var b [1]byte

	n, err := io.ReadFull(c, b[:])
	if n == 0 {
		r.m.noData.Add(1)
		r.log.Debug("connection closed without data", "remote", c.RemoteAddr().String(), "error", err)

		return nil, false
	}

	_ = c.SetReadDeadline(time.Time{})

	return &replayConn{Conn: c, r: io.MultiReader(bytes.NewReader(b[:]), c)}, true
}

func (r *route) handle(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()

	if r.cfg.WakeOn != wakeconfig.WakeOnConnect {
		// A probe that connects and closes must not wake the target or
		// refresh the activity tag: nothing is counted before a byte.
		wrapped, ok := r.awaitData(c)
		if !ok {
			return
		}

		c = wrapped
	}

	opened := r.now()
	first := r.open()

	defer func() {
		if r.closed() {
			r.touch(ctx, "", true)
		}
	}()

	remote := c.RemoteAddr().String()
	r.log.Info("connection open", "remote", remote, "active", r.activeNow())

	inst, err := r.resolve(ctx)
	if err != nil {
		r.m.connectFailures.Add(1)
		r.log.Error("connection refused: cannot resolve the target", "remote", remote, "error", err)

		return
	}

	if first {
		// Before anything else: another proxy that sees an old tag on a
		// target we are about to wake would stop it again.
		r.touch(ctx, inst.ID, true)
	}

	up, err := r.connect(ctx, inst)
	waited := r.now().Sub(opened)

	if err != nil {
		r.m.connectFailures.Add(1)
		r.log.Error("connection dropped: target unreachable", "remote", remote, "instance", inst.ID, "waited_ms", waited.Milliseconds(), "error", err)

		return
	}

	defer func() { _ = up.Close() }()

	r.p.track(up)
	defer r.p.untrack(up)

	r.log.Info("connection established", "remote", remote, "instance", inst.ID, "waited_ms", waited.Milliseconds())

	in, out := splice(c, up)

	r.log.Info("connection closed", "remote", remote, "instance", inst.ID, "duration_ms", r.now().Sub(opened).Milliseconds(), "bytes_in", in, "bytes_out", out)
}

func (r *route) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: r.p.opt.DialTimeout}

	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		r.m.dialFailures.Add(1)
	}

	return c, err
}

// connect returns a connection to the target, waking it when it sleeps and
// holding the caller for at most the wake timeout.
func (r *route) connect(ctx context.Context, inst Instance) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, r.p.cfg.WakeTimeout.D())
	defer cancel()

	if inst.State == StateRunning && inst.PrivateIP != "" {
		if c, err := r.dial(ctx, r.addr(inst)); err == nil {
			return c, nil
		}
	}

	f := r.join()

	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w (%s)", ErrWakeTimeout, r.p.cfg.WakeTimeout.D())
	}

	if f.err != nil {
		return nil, f.err
	}

	for {
		c, err := r.dial(ctx, f.addr)
		if err == nil {
			return c, nil
		}

		if !sleep(ctx, r.p.opt.DialRetry) {
			return nil, fmt.Errorf("%w: %w", ErrWakeTimeout, err)
		}
	}
}

// join returns the wake in progress, or starts one. Every client that needs
// a wake shares it, so the target is started once.
func (r *route) join() *flight {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wake != nil {
		return r.wake
	}

	f := &flight{done: make(chan struct{})}
	r.wake = f

	go r.runWake(f)

	return f
}

func (r *route) runWake(f *flight) {
	ctx, cancel := context.WithTimeout(r.p.connCtx, r.p.cfg.WakeTimeout.D())
	defer cancel()

	begin := r.now()
	addr, started, err := r.wakeLoop(ctx)

	if err != nil {
		r.m.wakeFailures.Add(1)
		r.log.Error("wake failed", "waited_ms", r.now().Sub(begin).Milliseconds(), "error", err)
	} else if started {
		d := r.now().Sub(begin)
		r.m.observeWake(d)
		r.log.Info("wake complete: target accepts connections", "duration_ms", d.Milliseconds(), "addr", addr)
	}

	r.mu.Lock()
	r.wake = nil
	r.mu.Unlock()

	f.addr, f.err = addr, err
	close(f.done)
}

// wakeLoop drives the target to running with an accepting port. It returns
// the address, and whether it started the instance itself.
func (r *route) wakeLoop(ctx context.Context) (addr string, started bool, err error) {
	var startedAt time.Time

	for {
		wait := r.p.opt.PollInterval

		inst, err := r.resolve(ctx)

		switch {
		case errors.Is(err, ErrNoTarget), errors.Is(err, ErrAmbiguous):
			return "", started, err
		case err != nil:
			r.log.Warn("describe failed; retrying", "error", err)
		default:
			switch inst.State {
			case StateRunning:
				wait = r.p.opt.DialRetry

				if inst.PrivateIP != "" {
					a := r.addr(inst)
					if c, derr := r.dial(ctx, a); derr == nil {
						_ = c.Close()

						return a, started, nil
					}
				}
			case StateStopped:
				if !started || r.now().Sub(startedAt) > r.p.opt.RestartAfter {
					if serr := r.startInstance(ctx, inst.ID); serr != nil {
						r.log.Warn("start failed; retrying", "instance", inst.ID, "error", serr)
					} else {
						started, startedAt = true, r.now()
						r.m.wakes.Add(1)
						r.log.Info("wake: start requested", "instance", inst.ID)
					}
				}
			default: // pending, stopping: wait for the state to settle.
			}
		}

		if !sleep(ctx, wait) {
			return "", started, fmt.Errorf("%w (%s)", ErrWakeTimeout, r.p.cfg.WakeTimeout.D())
		}
	}
}

func (r *route) startInstance(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, r.p.opt.APITimeout)
	defer cancel()

	return r.p.cloud.Start(ctx, id)
}

// touch writes the activity tag. Unless force, it writes at most once per
// ActivityInterval. An empty id is the last one resolved.
func (r *route) touch(ctx context.Context, id string, force bool) {
	r.mu.Lock()

	if id == "" {
		id = r.lastID
	}

	now := r.now()
	if id == "" || (!force && now.Sub(r.lastTouch) < r.p.opt.ActivityInterval) {
		r.mu.Unlock()

		return
	}

	r.lastTouch = now
	r.mu.Unlock()

	// The caller's context may be over (a last close during shutdown).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.p.opt.APITimeout)
	defer cancel()

	if err := r.p.cloud.Tag(ctx, id, r.p.cfg.ActivityTag, now.UTC().Format(time.RFC3339)); err != nil {
		r.log.Warn("activity tag not written", "instance", id, "error", err)
	}
}

func (r *route) activityLoop(ctx context.Context) {
	t := time.NewTicker(r.p.opt.ActivityInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.activeNow() > 0 {
				r.touch(ctx, "", false)
			}
		}
	}
}

func (r *route) idleLoop(ctx context.Context) {
	t := time.NewTicker(r.p.opt.IdleCheckInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.idleCheck(ctx)
		}
	}
}

// idleCheck stops the target when nobody has used it for the idle timeout:
// this proxy has no connection on the route, and the activity tag, which
// every proxy writes, is older than the timeout. A target without the tag is
// left alone until the timeout has passed since this proxy started.
func (r *route) idleCheck(ctx context.Context) {
	if r.activeNow() > 0 {
		return
	}

	inst, err := r.resolve(ctx)
	if err != nil {
		r.log.Warn("idle check: cannot resolve the target", "error", err)

		return
	}

	if inst.State != StateRunning {
		return
	}

	ref, source := r.p.started, "proxy start"

	if v, ok := inst.Tags[r.p.cfg.ActivityTag]; ok {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil {
			ref, source = t, "tag"
		} else {
			r.log.Warn("idle check: unreadable activity tag, counting from proxy start", "instance", inst.ID)
		}
	}

	idle := r.now().Sub(ref)
	if idle < r.p.cfg.IdleTimeout.D() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.active > 0 {
		return
	}

	sctx, cancel := context.WithTimeout(ctx, r.p.opt.APITimeout)
	defer cancel()

	if err := r.p.cloud.Stop(sctx, inst.ID); err != nil {
		r.log.Error("stop failed", "instance", inst.ID, "error", err)

		return
	}

	r.m.stops.Add(1)
	r.log.Info("stop: target idle", "instance", inst.ID, "idle_ms", idle.Milliseconds(), "since", source)
}
