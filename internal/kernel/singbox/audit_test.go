package singbox

import (
	"context"
	"fmt"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"golang.org/x/time/rate"
	"reflect"
	"sync"
	"testing"
)

func TestNaiveAccountingRevocationAndDeviceLimits(t *testing.T) {
	tr := NewConnTracker(0)
	users := []model.UserSpec{{ID: 1, UUID: "one"}, {ID: 2, UUID: "two"}}
	setTrackerUsers(tr, "naive", users)
	var checked []string
	tr.SetDeviceLimitFunc(func(uuid string) (int, bool) { checked = append(checked, uuid); return 1, true })
	tr.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid != "one" && uuid != "two" {
			t.Errorf("bad speed lookup %q", uuid)
		}
		return nil
	})
	var conns []*testConn
	for i, u := range users {
		c := &testConn{reads: [][]byte{[]byte("abc")}}
		conns = append(conns, c)
		wrapper := tr.RoutedConnection(context.Background(), c, testInboundContext(fmt.Sprint(u.ID), fmt.Sprintf("127.0.0.%d", i+1)), nil, nil)
		var b [3]byte
		if _, err := wrapper.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := wrapper.Write(b[:i+1]); err != nil {
			t.Fatal(err)
		}
	}
	traffic, ips, count := tr.GetUserTraffic()
	if !reflect.DeepEqual(checked, []string{"one", "two"}) || traffic[1] != [2]int64{3, 1} || traffic[2] != [2]int64{3, 2} || count != 2 || !ips[1]["127.0.0.1"] || !ips[2]["127.0.0.2"] {
		t.Fatalf("traffic=%v ips=%v count=%d", traffic, ips, count)
	}
	setTrackerUsers(tr, "naive", users[1:])
	if !conns[0].closed || conns[1].closed || tr.ActiveCount() != 1 {
		t.Fatal("wrong user drained")
	}
}
func TestConcurrentLocalDeviceAdmission(t *testing.T) {
	tr := NewConnTracker(0)
	tr.SetUserMap(map[string]int{"u": 1})
	tr.SetDeviceLimitFunc(func(string) (int, bool) { return 1, true })
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.RoutedConnection(context.Background(), &testConn{}, testInboundContext("u", fmt.Sprintf("127.0.0.%d", i+1)), nil, nil)
		}()
	}
	wg.Wait()
	if tr.ActiveCount() != 1 {
		t.Fatalf("admitted %d", tr.ActiveCount())
	}
}
func TestRealityTargetsAndHY2Alias(t *testing.T) {
	for _, v := range []struct {
		dest, host string
		port       int
	}{{"[2001:db8::1]:8443", "2001:db8::1", 8443}, {"2001:db8::1", "2001:db8::1", 443}, {"example.com:444", "example.com", 444}} {
		n := &model.NodeSpec{TLSSettings: map[string]any{"dest": v.dest}}
		h := buildRealityConfig(n)["reality"].(M)["handshake"].(M)
		if h["server"] != v.host || h["server_port"] != v.port {
			t.Fatalf("%s: %v", v.dest, h)
		}
	}
	n := model.NodeSpec{Protocol: "hysteria", Version: 2, ServerPort: 8443}
	a := buildInbound(&n, nil, kernel.TLSCert{})
	n.Protocol = "hysteria2"
	n.Version = 0
	if b := buildInbound(&n, nil, kernel.TLSCert{}); !reflect.DeepEqual(a, b) {
		t.Fatalf("alias yields %v, want %v", b, a)
	}
}
func TestBlankPortsDoNotCreateUnconditionalRule(t *testing.T) {
	r := model.CustomRouteRule{Match: model.RouteMatch{Domains: []string{"example.test"}, Ports: []string{" "}, SourcePorts: []string{""}}}
	if rules := compileCustomRouteRule(r); len(rules) != 1 || rules[0]["domain"] == nil {
		t.Fatalf("%v", rules)
	}
}
