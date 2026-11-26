package kernel

import (
	"sync/atomic"

	"github.com/cedar2025/xboard-node/internal/model"
)

// UserPolicy publishes the credentials and device caps that may open a managed
// session. A transport may have installed candidate credentials before the
// control plane has confirmed the whole update.
type UserPolicy struct {
	users atomic.Pointer[map[int]model.UserSpec]
}

func (p *UserPolicy) Publish(users []model.UserSpec) {
	snapshot := make(map[int]model.UserSpec, len(users))
	for _, user := range users {
		snapshot[user.ID] = user
	}
	p.users.Store(&snapshot)
}

// Xray exposes the authenticated account ID. sing-box also exposes the UUID.
func (p *UserPolicy) DeviceLimit(id int, uuid string) (int, bool) {
	snapshot := p.users.Load()
	if snapshot == nil {
		return 0, false
	}
	user, ok := (*snapshot)[id]
	return user.DeviceLimit, ok && (uuid == "" || uuid == user.UUID)
}
