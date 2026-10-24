package registration

import (
	"context"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/lifecycle"
)

func TestRun_StopsOnCancelWhileBrokerUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Port 1 refuses connections, so Run sits in its retry backoff.
		Run(ctx, "amqp://guest:guest@127.0.0.1:1/", lifecycle.Identity{
			ModuleType: "channel", ModuleName: "http", Instance: "test", RPCQueue: "q",
		}, nil)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
