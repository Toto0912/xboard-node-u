package xray

import (
	"context"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	xrayCore "github.com/xtls/xray-core/core"
	bandwidth "github.com/xtls/xray-core/features/bandwidth"
	"golang.org/x/time/rate"
	"testing"
)

func TestXrayBandwidthUpdateRetainsUnchangedMappings(t *testing.T) {
	x := New(config.KernelConfig{})
	bm := bandwidth.New()
	inst := new(xrayCore.Instance)
	if err := inst.AddFeature(bm); err != nil {
		t.Fatal(err)
	}
	x.instance = inst
	users := []model.UserSpec{{ID: 1, UUID: "first", SpeedLimit: 1}, {ID: 2, UUID: "second", SpeedLimit: 2}}
	x.updateBandwidthLimits(users)
	want := bm.GetUserLimiter(userEmail(2))
	if want == nil {
		t.Fatal("missing original limiter")
	}
	x.speedLimitFunc = func(uuid string) *rate.Limiter {
		if bm.GetUserLimiter(userEmail(2)) != want {
			t.Fatal("unchanged user lost its limiter while the update was in progress")
		}
		if uuid == "second" {
			return want
		}
		return nil
	}
	x.updateBandwidthLimits(users[1:])
	if bm.GetUserLimiter(userEmail(1)) != nil || bm.GetUserLimiter(userEmail(2)) != want {
		t.Fatal("unexpected final limiter map")
	}
}

func TestShadowsocksColdAndHotCipherSupport(t *testing.T) {
	for _, cipher := range []string{"aes-128-gcm", "AES-256-GCM", "aead_aes_128_gcm", "chacha20-ietf-poly1305", "aead_xchacha20_poly1305", "none", "plain"} {
		t.Run(cipher, func(t *testing.T) {
			user := model.UserSpec{ID: 1, UUID: "fixture-password"}
			n := &model.NodeSpec{Protocol: "shadowsocks", ListenIP: "127.0.0.1", ServerPort: 12345, Cipher: cipher}
			_, cold := parseConfig(context.Background(), config.KernelConfig{}, n, []model.UserSpec{user}, kernel.TLSCert{}, false)
			_, hot := toMemoryUser("shadowsocks", n, user)
			supported := cipher != "none" && cipher != "plain"
			if (cold == nil) != supported || (hot == nil) != supported {
				t.Fatalf("expected supported=%t, cold=%v hot=%v", supported, cold, hot)
			}
		})
	}
}
