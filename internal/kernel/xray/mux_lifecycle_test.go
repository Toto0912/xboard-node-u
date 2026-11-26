package xray

import (
	"testing"
	"time"

	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestMuxClientShutdownUnblocksOutboundReader(t *testing.T) {
	downlink, downstream := pipe.New()
	uplink, upstream := pipe.New()
	defer downstream.Interrupt()
	defer upstream.Interrupt()
	worker, err := mux.NewClientWorker(transport.Link{Reader: downlink, Writer: upstream}, mux.ClientStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		_, err := uplink.ReadMultiBuffer()
		closed <- err
	}()
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("outbound reader remained readable after mux shutdown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mux shutdown left the outbound reader blocked")
	}
}
