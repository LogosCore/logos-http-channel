// Package registration keeps this channel instance registered with core over
// the AMQP control plane. Core rejects sync traffic from instances that are not
// registered, so the channel is only useful while this loop is healthy.
package registration

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/lifecycle"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// Run registers id with core and keeps it alive with heartbeats until ctx is
// cancelled, then deregisters. Lifecycle.Client gives up on a failed initial
// register and never recovers a dropped broker connection, so Run supervises
// it: every failure reconnects with exponential backoff.
func Run(ctx context.Context, amqpURL string, id lifecycle.Identity) {
	backoff := minBackoff
	for {
		started := time.Now()
		err := runSession(ctx, amqpURL, id)
		if ctx.Err() != nil {
			return
		}
		// A session that stayed up for a while was healthy; start over fast.
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		log.Printf("registration: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runSession runs one broker connection's worth of lifecycle. It returns nil
// only on ctx cancellation (after deregistering).
func runSession(ctx context.Context, amqpURL string, id lifecycle.Identity) error {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("connect to RabbitMQ: %w", err)
	}
	defer conn.Close()

	pub, err := lifecycle.NewAMQPPublisher(conn)
	if err != nil {
		return fmt.Errorf("open lifecycle publisher: %w", err)
	}
	defer pub.Close()

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	connLost := make(chan error, 1)
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	go func() {
		select {
		case e := <-closed:
			connLost <- fmt.Errorf("RabbitMQ connection closed: %v", e)
			cancel()
		case <-sessionCtx.Done():
		}
	}()

	client := lifecycle.NewClient(loggingPublisher{pub: pub, instance: id.Instance, connClosed: conn.IsClosed}, id)
	if err := client.Run(sessionCtx); err != nil {
		return err
	}
	select {
	case err := <-connLost:
		return err
	default:
		return nil
	}
}

// loggingPublisher logs lifecycle outcomes. lifecycle.Client swallows heartbeat
// errors and exposes no hooks, so without this a rejected or unreachable
// channel would fail silently.
type loggingPublisher struct {
	pub      lifecycle.Publisher
	instance string
	// connClosed suppresses failures of the deregister lifecycle.Client
	// attempts after the connection is already gone; Run reports that loss.
	connClosed func() bool
}

func (p loggingPublisher) Call(ctx context.Context, req protocol.Envelope) (protocol.ReplyEnvelope, error) {
	reply, err := p.pub.Call(ctx, req)
	switch {
	case err != nil:
		if ctx.Err() == nil && !p.connClosed() {
			log.Printf("registration: %s failed: %v", req.Type, err)
		}
	case reply.Status == protocol.StatusError:
		code := "unknown"
		if reply.Error != nil {
			code = reply.Error.Code + ": " + reply.Error.Message
		}
		log.Printf("registration: %s rejected by core (%s)", req.Type, code)
	case req.Type == lifecycle.TypeRegister:
		log.Printf("registration: registered with core as %q", p.instance)
	case req.Type == lifecycle.TypeDeregister:
		log.Printf("registration: deregistered %q", p.instance)
	}
	return reply, err
}
