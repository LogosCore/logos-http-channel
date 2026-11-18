package httpserver

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// A slow link, simulated (SIMULATE_LATENCY): to reproduce what minions see
// over one, a handler built with WithSimulatedLatency delays every message it
// carries, each way, by its RuntimeConfig.Latency. A plain request reaches
// core a delay late and its answer, whichever it is, takes another to get
// back; a WebSocket's handshake answers a round trip late, and each frame,
// the minion's and core's, goes on a delay after it came. A WebSocket's
// frames keep their order, as a TCP connection's bytes do: one never
// overtakes the one before it, whatever the jitter, and a burst is not
// spread out (each frame waits its own delay, not the ones before it). A
// close, either side's, reaches the other after the frames sent before it.
// Pings are answered at once. Frames with no delay and none ahead of them
// go straight on, as through a handler built without the option, which
// never delays anything.

// Latency is the delay a handler built with WithSimulatedLatency adds to
// every message it carries; the zero value adds none.
type Latency struct {
	// Delay is added to each message each way: minion to core, core to the
	// minion. A round trip through the channel takes twice it longer.
	Delay time.Duration
	// Jitter spreads each delay at random by up to this percent of it,
	// either way (0 to 100).
	Jitter int
}

// next is one message's delay: Delay spread by Jitter.
func (l Latency) next() time.Duration {
	if l.Delay <= 0 {
		return 0
	}
	spread := float64(l.Delay) * float64(min(max(l.Jitter, 0), 100)) / 100
	return time.Duration(max(0, float64(l.Delay)+(rand.Float64()*2-1)*spread))
}

// Option changes how NewHandler builds a handler.
type Option func(*Handler)

// WithSimulatedLatency builds a handler that delays the messages it carries
// as its RuntimeConfig.Latency says. Without it the latency is never applied.
func WithSimulatedLatency() Option {
	return func(h *Handler) { h.simulate = true }
}

// SimulatesLatency reports whether h was built with WithSimulatedLatency.
func (h *Handler) SimulatesLatency() bool { return h.simulate }

// lag is the delay of one message under cfg: none when h does not simulate
// latency.
func (h *Handler) lag(cfg *RuntimeConfig) time.Duration {
	if !h.simulate {
		return 0
	}
	return cfg.Latency.next()
}

// sleep waits d, or until ctx ends: false then.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// lagFrames is the room a WebSocket's delay line has in each direction:
// beyond it the side putting waits, as a full link pushes back on its
// sender.
const lagFrames = 64

// delayLine hands items on in order, each once its time has come: when it
// was put plus its delay, and never before the item ahead of it (one
// goroutine takes them first in, first out). Whoever puts may hand an item
// on itself instead while the line is not busy: then nothing is ahead of
// it.
type delayLine[T any] struct {
	items chan timed[T]
	// pending counts the items put and not yet handed on (marks too): one
	// leaves once fn returned for it.
	pending atomic.Int64
}

type timed[T any] struct {
	at   time.Time
	item T
	// mark, set, is no item but a place in the line: closed once the items
	// ahead of it went on and its own time came.
	mark chan struct{}
}

func newDelayLine[T any]() *delayLine[T] {
	return &delayLine[T]{items: make(chan timed[T], lagFrames)}
}

// busy reports whether items are on their way.
func (l *delayLine[T]) busy() bool { return l.pending.Load() > 0 }

// put queues item to go on after delay, waiting while the line is full;
// false when ctx ended first.
func (l *delayLine[T]) put(ctx context.Context, item T, delay time.Duration) bool {
	return l.enqueue(ctx, timed[T]{item: item}, delay)
}

// after is closed once the items put before it went on, and delay more;
// nil when ctx ended first.
func (l *delayLine[T]) after(ctx context.Context, delay time.Duration) <-chan struct{} {
	mark := make(chan struct{})
	if !l.enqueue(ctx, timed[T]{mark: mark}, delay) {
		return nil
	}
	return mark
}

func (l *delayLine[T]) enqueue(ctx context.Context, it timed[T], delay time.Duration) bool {
	it.at = time.Now().Add(delay)
	l.pending.Add(1)
	select {
	case l.items <- it:
		return true
	case <-ctx.Done():
		l.pending.Add(-1)
		return false
	}
}

// run hands each item to fn at its time, in order, until ctx ends or fn
// returns false.
func (l *delayLine[T]) run(ctx context.Context, fn func(T) bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-l.items:
			if !sleep(ctx, time.Until(it.at)) {
				return
			}
			ok := true
			if it.mark != nil {
				close(it.mark)
			} else {
				ok = fn(it.item)
			}
			l.pending.Add(-1)
			if !ok {
				return
			}
		}
	}
}

// wait waits for c, or until ctx ends: false then, or for a nil c.
func wait(ctx context.Context, c <-chan struct{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c:
		return true
	case <-ctx.Done():
		return false
	}
}
