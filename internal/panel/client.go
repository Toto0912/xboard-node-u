package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/go-viper/mapstructure/v2"
)

var (
	ErrIncompatibleConfig = errors.New("incompatible node configuration")
	ErrNodeRevoked        = errors.New("panel revoked node binding")
	trafficMapPool        = sync.Pool{New: func() interface{} { return make(map[string][2]int64) }}
	aliveMapPool          = sync.Pool{New: func() interface{} { return make(map[string][]string) }}
	onlineMapPool         = sync.Pool{New: func() interface{} { return make(map[string]int) }}
)

// Client communicates with the Xboard panel API.
// When machineID > 0 the client uses machine-level authentication
// (machine_id + token) instead of the legacy (token + node_type) scheme.
type Client struct {
	baseURL         string
	token           string
	nodeID          int
	nodeType        string
	machineID       int
	httpClient      *http.Client
	batchCapability *atomic.Int32 // shared with all child clients of a machine

	etagMu     sync.Mutex
	configETag string
	userETag   string

	apiSuccess atomic.Uint64
	apiFailure atomic.Uint64
}

func (c *Client) CloseIdleConnections() { c.httpClient.CloseIdleConnections() }

// NewClient creates a new panel API client.
func NewClient(cfg config.PanelConfig) *Client {
	return &Client{
		baseURL:         strings.TrimRight(cfg.URL, "/"),
		batchCapability: new(atomic.Int32),
		token:           cfg.Token,
		nodeID:          cfg.NodeID,
		nodeType:        cfg.NodeType,
		machineID:       cfg.MachineID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ForNode returns a new client bound to a specific node_id, sharing the
// same base URL, auth and HTTP transport. ETags and metric counters start fresh.
func (c *Client) ForNode(nodeID int) *Client {
	return &Client{
		baseURL:         c.baseURL,
		batchCapability: c.batchCapability,
		token:           c.token,
		nodeID:          nodeID,
		nodeType:        c.nodeType,
		machineID:       c.machineID,
		httpClient:      c.httpClient,
	}
}

// ResetConfigETag clears the cached config ETag so the next GetConfig call
// returns a full response instead of 304. Used by machine mode after a
// pre-fetch to probe the transport type.
func (c *Client) ResetConfigETag() {
	c.etagMu.Lock()
	c.configETag = ""
	c.etagMu.Unlock()
}

// ResetUserETag clears the cached user ETag so the next GetUsers call returns
// an authoritative full user snapshot instead of 304.
func (c *Client) ResetUserETag() {
	c.etagMu.Lock()
	c.userETag = ""
	c.etagMu.Unlock()
}

// Handshake calls the new v2 API to get WS config + initial data in one shot.
func (c *Client) Handshake() (*HandshakeResponse, error) {
	return c.HandshakeContext(context.Background())
}

func (c *Client) HandshakeContext(ctx context.Context) (*HandshakeResponse, error) {
	resp, err := c.doRequestContext(ctx, "POST", "/api/v2/server/handshake", nil, "")
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("handshake status %d: %s", resp.StatusCode, body)
	}

	var hs HandshakeResponse
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		return nil, fmt.Errorf("decode handshake: %w", err)
	}
	c.setBatchCapability(hs.Capabilities)
	return &hs, nil
}

// Report sends consolidated traffic + alive + status data to the panel.
// The optional metrics map allows the node to submit richer telemetry
// (active connections, per-core CPU, GC stats, limiter hits, etc.)
// without changing the core schema of status.
func (c *Client) Report(traffic map[int][2]int64, alive map[int][]string, online map[int]int,
	cpu float64, mem, swap, disk [2]uint64,
	metrics map[string]interface{},
) error {
	payload := make(map[string]interface{})

	if len(traffic) > 0 {
		t := trafficMapPool.Get().(map[string][2]int64)
		for uid, d := range traffic {
			t[strconv.Itoa(uid)] = d
		}
		payload["traffic"] = t
		defer func() {
			for k := range t {
				delete(t, k)
			}
			trafficMapPool.Put(t)
		}()
	}

	if alive != nil {
		a := aliveMapPool.Get().(map[string][]string)
		for uid, ips := range alive {
			a[strconv.Itoa(uid)] = ips
		}
		payload["alive"] = a
		defer func() {
			for k := range a {
				delete(a, k)
			}
			aliveMapPool.Put(a)
		}()
	}

	if len(online) > 0 {
		o := onlineMapPool.Get().(map[string]int)
		for uid, count := range online {
			o[strconv.Itoa(uid)] = count
		}
		payload["online"] = o
		defer func() {
			for k := range o {
				delete(o, k)
			}
			onlineMapPool.Put(o)
		}()
	}

	status := map[string]interface{}{
		"cpu":  cpu,
		"mem":  map[string]interface{}{"total": mem[0], "used": mem[1]},
		"swap": map[string]interface{}{"total": swap[0], "used": swap[1]},
		"disk": map[string]interface{}{"total": disk[0], "used": disk[1]},
	}
	payload["status"] = status

	if len(metrics) > 0 {
		payload["metrics"] = metrics
	}

	return c.postJSON("/api/v2/server/report", payload)
}

// decodeWeakRaw decodes an interface (from JSON map) into a struct using weak type conversion.
func decodeWeakRaw(input map[string]interface{}, output interface{}) error {
	config := &mapstructure.DecoderConfig{
		Metadata:         nil,
		Result:           output,
		WeaklyTypedInput: true,
		TagName:          "json",
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			func(f, t reflect.Type, v interface{}) (interface{}, error) {
				if t == reflect.TypeOf(OutboundConfig{}) && f.Kind() == reflect.Map {
					// mapstructure does not invoke json.Unmarshaler. Use the
					// same lossless codec for REST and nested WS payloads.
					data, err := json.Marshal(v)
					if err != nil {
						return nil, err
					}
					var outbound OutboundConfig
					if err := json.Unmarshal(data, &outbound); err != nil {
						return nil, err
					}
					return outbound, nil
				}
				// Handle []interface{} -> StringOrArray
				if t == reflect.TypeOf(StringOrArray("")) && f.Kind() == reflect.Slice {
					arr, _ := v.([]interface{})
					var strs []string
					for _, x := range arr {
						if s, ok := x.(string); ok {
							strs = append(strs, s)
						}
					}
					return StringOrArray(strings.Join(strs, "\n")), nil
				}
				return v, nil
			},
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	decoder, _ := mapstructure.NewDecoder(config)
	return decoder.Decode(input)
}

// configPath returns the API path for config fetching.
// Machine mode uses the v2 endpoint; legacy mode uses the v1 UniProxy endpoint.
func (c *Client) configPath() string {
	if c.machineID > 0 {
		return "/api/v2/server/config"
	}
	return "/api/v1/server/UniProxy/config"
}

// userPath returns the API path for user fetching.
func (c *Client) userPath() string {
	if c.machineID > 0 {
		return "/api/v2/server/user"
	}
	return "/api/v1/server/UniProxy/user"
}

// GetConfig fetches node configuration and commits its ETag immediately. Returns nil if not modified (304).
func (c *Client) GetConfig() (*NodeConfig, error) {
	cfg, etag, err := c.GetConfigWithETag()
	if err == nil {
		c.CommitConfigETag(etag)
	}
	return cfg, err
}

// GetConfigWithETag fetches node configuration and returns any new ETag without saving it yet.
func (c *Client) GetConfigWithETag() (*NodeConfig, string, error) {
	return c.GetConfigContext(context.Background())
}

func (c *Client) GetConfigContext(ctx context.Context) (*NodeConfig, string, error) {
	c.etagMu.Lock()
	etag := c.configETag
	c.etagMu.Unlock()

	resp, err := c.doRequestContext(ctx, "GET", c.configPath(), nil, etag)
	if err != nil {
		return nil, "", fmt.Errorf("get config: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotModified {
		return nil, "", nil
	}
	if c.machineID == 0 && resp.StatusCode == http.StatusGone && resp.Header.Get("X-Xboard-Node-State") == "revoked" {
		return nil, "", ErrNodeRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: HTTP %d %s", c.configPath(), resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read config response: %w", err)
	}
	cfg, err := decodeNodeConfig(body)
	if err != nil {
		return nil, "", err
	}
	return cfg, resp.Header.Get("ETag"), nil
}

func decodeNodeConfig(body []byte) (*NodeConfig, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON object", ErrIncompatibleConfig)
	}
	var cfg NodeConfig
	// Use mapstructure for weak type conversion (string -> int, bool -> string, etc.)
	if err := decodeWeakRaw(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%w: invalid field types", ErrIncompatibleConfig)
	}

	// Basic validation
	if cfg.Protocol == "" {
		return nil, fmt.Errorf("%w: missing protocol", ErrIncompatibleConfig)
	}

	return &cfg, nil
}

// CommitConfigETag saves a config ETag after callers have processed the snapshot.
func (c *Client) CommitConfigETag(etag string) {
	if etag != "" {
		c.etagMu.Lock()
		c.configETag = etag
		c.etagMu.Unlock()
	}
}

// GetUsers fetches available users and commits their ETag immediately. Returns nil if not modified (304).
func (c *Client) GetUsers() ([]User, error) {
	users, etag, err := c.GetUsersWithETag()
	if err == nil {
		c.CommitUserETag(etag)
	}
	return users, err
}

// GetUsersWithETag fetches available users and returns any new ETag without saving it yet.
func (c *Client) GetUsersWithETag() ([]User, string, error) {
	return c.GetUsersContext(context.Background())
}

func (c *Client) GetUsersContext(ctx context.Context) ([]User, string, error) {
	c.etagMu.Lock()
	etag := c.userETag
	c.etagMu.Unlock()

	resp, err := c.doRequestContext(ctx, "GET", c.userPath(), nil, etag)
	if err != nil {
		return nil, "", fmt.Errorf("get users: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotModified {
		return nil, "", nil
	}
	if c.machineID == 0 && resp.StatusCode == http.StatusGone && resp.Header.Get("X-Xboard-Node-State") == "revoked" {
		return nil, "", ErrNodeRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: HTTP %d %s", c.userPath(), resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	var usersResp UsersResponse
	if err := json.NewDecoder(resp.Body).Decode(&usersResp); err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	return usersResp.Users, resp.Header.Get("ETag"), nil
}

// CommitUserETag saves a users ETag after callers have processed the snapshot.
func (c *Client) CommitUserETag(etag string) {
	if etag != "" {
		c.etagMu.Lock()
		c.userETag = etag
		c.etagMu.Unlock()
	}
}

// PushTraffic submits per-user traffic data
func (c *Client) PushTraffic(data map[int][2]int64) error {
	if len(data) == 0 {
		return nil
	}
	payload := make(map[string]interface{}, len(data))
	for uid, traffic := range data {
		payload[strconv.Itoa(uid)] = traffic
	}
	return c.postJSON("/api/v1/server/UniProxy/push", payload)
}

// PushAlive submits online user IPs
func (c *Client) PushAlive(data map[int][]string) error {
	if len(data) == 0 {
		return nil
	}
	payload := make(map[string]interface{}, len(data))
	for uid, ips := range data {
		payload[strconv.Itoa(uid)] = ips
	}
	return c.postJSON("/api/v1/server/UniProxy/alive", payload)
}

// PushStatus submits system status to the panel
func (c *Client) PushStatus(cpu float64, mem, swap, disk [2]uint64) error {
	payload := map[string]interface{}{
		"cpu":  cpu,
		"mem":  map[string]interface{}{"total": mem[0], "used": mem[1]},
		"swap": map[string]interface{}{"total": swap[0], "used": swap[1]},
		"disk": map[string]interface{}{"total": disk[0], "used": disk[1]},
	}
	return c.postJSON("/api/v1/server/UniProxy/status", payload)
}

// ResetETags clears cached ETags, forcing full responses
func (c *Client) ResetETags() {
	c.etagMu.Lock()
	c.configETag = ""
	c.userETag = ""
	c.etagMu.Unlock()
}

// ErrMachineUnauthorized means the discovery endpoint explicitly rejected the machine.
var ErrMachineUnauthorized = errors.New("machine authorization rejected")

// GetMachineNodes fetches the list of active nodes bound to this machine.
func (c *Client) GetMachineNodes() (*MachineNodesResponse, error) {
	return c.GetMachineNodesContext(context.Background())
}

func (c *Client) GetMachineNodesContext(ctx context.Context) (*MachineNodesResponse, error) {
	resp, err := c.doRequestContext(ctx, "POST", "/api/v2/server/machine/nodes", nil, "")
	if err != nil {
		return nil, fmt.Errorf("get machine nodes: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: status %d", ErrMachineUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("machine nodes status %d: %s", resp.StatusCode, body)
	}

	var out MachineNodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode machine nodes: %w", err)
	}
	if out.Nodes == nil {
		return nil, errors.New("machine discovery response missing nodes array")
	}
	for _, node := range out.Nodes {
		if node.ID <= 0 || node.Type == "" {
			return nil, errors.New("machine discovery response contains invalid node")
		}
	}
	c.setBatchCapability(out.Capabilities)
	return &out, nil
}

// ReportMachineStatus sends machine-level load metrics to the panel.
// netIn/netOut are bytes/sec; negative values mean "unavailable" (first sample).
func (c *Client) ReportMachineStatus(cpu float64, mem, swap, disk [2]uint64, netIn, netOut float64) error {
	return c.ReportMachineStatusContext(context.Background(), cpu, mem, swap, disk, netIn, netOut)
}

func (c *Client) ReportMachineStatusContext(ctx context.Context, cpu float64, mem, swap, disk [2]uint64, netIn, netOut float64) error {
	payload := map[string]interface{}{
		"cpu":  cpu,
		"mem":  map[string]interface{}{"total": mem[0], "used": mem[1]},
		"swap": map[string]interface{}{"total": swap[0], "used": swap[1]},
		"disk": map[string]interface{}{"total": disk[0], "used": disk[1]},
	}
	if netIn >= 0 && netOut >= 0 {
		payload["net"] = map[string]interface{}{"in_speed": netIn, "out_speed": netOut}
	}
	return c.postJSONContext(ctx, "/api/v2/server/machine/status", payload)
}

// injectAuth writes authentication fields into a payload map.
func (c *Client) injectAuth(m map[string]interface{}) {
	m["token"] = c.token
	if c.machineID > 0 {
		m["machine_id"] = c.machineID
		if c.nodeID > 0 {
			m["node_id"] = c.nodeID
		}
	} else {
		m["node_id"] = c.nodeID
		if c.nodeType != "" {
			m["node_type"] = c.nodeType
		}
	}
}

// authQuery builds URL query parameters for GET requests.
func (c *Client) authQuery() url.Values {
	q := url.Values{}
	q.Set("token", c.token)
	if c.machineID > 0 {
		q.Set("machine_id", strconv.Itoa(c.machineID))
		if c.nodeID > 0 {
			q.Set("node_id", strconv.Itoa(c.nodeID))
		}
	} else {
		q.Set("node_id", strconv.Itoa(c.nodeID))
		if c.nodeType != "" {
			q.Set("node_type", c.nodeType)
		}
	}
	return q
}

// postJSON marshals a map payload (with auth fields injected) and POSTs it.
func (c *Client) postJSON(path string, payload map[string]interface{}) error {
	return c.postJSONContext(context.Background(), path, payload)
}

func (c *Client) postJSONContext(ctx context.Context, path string, payload map[string]interface{}) error {
	c.injectAuth(payload)

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	resp, err := c.doRequestContext(ctx, "POST", path, body, "")
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

func (c *Client) doRequest(method, path string, body []byte, ifNoneMatch string) (*http.Response, error) {
	return c.doRequestContext(context.Background(), method, path, body, ifNoneMatch)
}

func (c *Client) doRequestContext(ctx context.Context, method, path string, body []byte, ifNoneMatch string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else if method != http.MethodGet {
		authOnly := make(map[string]interface{})
		c.injectAuth(authOnly)
		merged, _ := json.Marshal(authOnly)
		bodyReader = bytes.NewReader(merged)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, safeRequestError(method, path, err)
	}
	if method == http.MethodGet {
		req.URL.RawQuery = c.authQuery().Encode()
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	nlog.Core().Debug("panel request", "method", method, "path", path)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.apiFailure.Add(1)
		return nil, safeRequestError(method, path, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		c.apiSuccess.Add(1)
	} else {
		c.apiFailure.Add(1)
	}

	nlog.Core().Debug("panel response", "method", method, "path", path, "status", resp.StatusCode)
	return resp, nil
}

// net/http includes the authenticated URL in transport errors. A malformed
// redirect can also quote its query inside the underlying error message.
func safeRequestError(method, path string, err error) error {
	var urlErr *url.Error
	for errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if before, _, found := strings.Cut(err.Error(), "?"); found {
		err = &queryRedactedError{cause: err, message: before + " [query omitted]"}
	}
	return &url.Error{Op: method, URL: path, Err: err}
}

type queryRedactedError struct {
	cause   error
	message string
}

func (e *queryRedactedError) Error() string { return e.message }
func (e *queryRedactedError) Unwrap() error { return e.cause }

// drainAndClose reads any remaining bytes (up to 512B) and closes the body.
// This allows the HTTP transport to reuse the underlying TCP connection.
func drainAndClose(body io.ReadCloser) {
	io.CopyN(io.Discard, body, 512)
	body.Close()
}

// APIMetrics counts HTTP outcomes before individual endpoints validate their payloads.
type APIMetrics struct {
	Success uint64
	Failure uint64
}

// SnapshotMetrics returns a snapshot of API metrics.
func (c *Client) SnapshotMetrics() APIMetrics {
	return APIMetrics{
		Success: c.apiSuccess.Load(),
		Failure: c.apiFailure.Load(),
	}
}
