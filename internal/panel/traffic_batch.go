package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type TrafficBatch struct {
	ID      string     `json:"batch_id"`
	Traffic [][3]int64 `json:"traffic"`
}

func (c *Client) setBatchCapability(caps Capabilities) {
	if caps.TrafficBatchV1 == nil {
		return // an older panel in a rolling deployment cannot revoke negotiation
	}
	value := int32(-1)
	if *caps.TrafficBatchV1 {
		value = 1
	}
	c.batchCapability.Store(value)
}

// TrafficBatchCapability distinguishes an older panel from an explicit opt-out.
func (c *Client) TrafficBatchCapability() (enabled, known bool) {
	v := c.batchCapability.Load()
	return v > 0, v != 0
}

// ErrBatchRejected denotes a batch-specific conflict or validation failure.
var ErrBatchRejected = errors.New("traffic batch rejected")

// SendTrafficBatch returns success only for the matching durable receipt.
func (c *Client) SendTrafficBatch(ctx context.Context, b TrafficBatch) error {
	payload := map[string]interface{}{"batch_id": b.ID, "traffic": b.Traffic}
	c.injectAuth(payload)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := c.doRequestContext(ctx, "POST", "/api/v2/server/traffic-batch", body, "")
	if err != nil {
		return err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %s HTTP %d", ErrBatchRejected, b.ID, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("traffic batch %s: HTTP %d", b.ID, resp.StatusCode)
	}
	var ack struct {
		ID string `json:"batch_id"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &ack) != nil || ack.ID != b.ID {
		return fmt.Errorf("traffic batch %s: missing or mismatched receipt", b.ID)
	}
	return nil
}
