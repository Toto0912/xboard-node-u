package model

// IntersectUsers returns credentials accepted by both snapshots, in left's order.
// While snapshots disagree, keep the tighter positive limit (zero means unlimited).
// An empty intersection is an explicit empty list, not a missing snapshot.
func IntersectUsers(left, right []UserSpec) []UserSpec {
	if len(left) == 0 || len(right) == 0 {
		return []UserSpec{}
	}
	allowed := make(map[int]UserSpec, len(right))
	for _, user := range right {
		allowed[user.ID] = user
	}
	users := make([]UserSpec, 0, min(len(left), len(right)))
	for _, user := range left {
		other, ok := allowed[user.ID]
		if !ok || user.UUID != other.UUID {
			continue
		}
		user.SpeedLimit = tighterLimit(user.SpeedLimit, other.SpeedLimit)
		user.DeviceLimit = tighterLimit(user.DeviceLimit, other.DeviceLimit)
		users = append(users, user)
	}
	return users
}

func tighterLimit(a, b int) int {
	if a <= 0 {
		return max(b, 0)
	}
	if b <= 0 {
		return a
	}
	return min(a, b)
}
