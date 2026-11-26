package kernel

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
)

func TestUserDiffUUIDChangeReturnsOldAndNew(t *testing.T) {
	oldUsers := []model.UserSpec{
		{ID: 1, UUID: "uuid-1"},
		{ID: 2, UUID: "uuid-2-old"},
	}
	newUsers := []model.UserSpec{
		{ID: 1, UUID: "uuid-1"},
		{ID: 2, UUID: "uuid-2-new"},
	}

	toAdd, toRemove := UserDiff(oldUsers, newUsers)
	if len(toAdd) != 1 || toAdd[0].ID != 2 || toAdd[0].UUID != "uuid-2-new" {
		t.Fatalf("toAdd = %#v, want [{ID:2 UUID:uuid-2-new}]", toAdd)
	}
	if len(toRemove) != 1 || toRemove[0].ID != 2 || toRemove[0].UUID != "uuid-2-old" {
		t.Fatalf("toRemove = %#v, want [{ID:2 UUID:uuid-2-old}]", toRemove)
	}
}
