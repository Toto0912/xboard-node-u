package model

import (
	"slices"
	"testing"
)

func TestIntersectUsersKeepsOnlySharedCredentialsAndTighterLimits(t *testing.T) {
	left := []UserSpec{
		{ID: 1, UUID: "a", DeviceLimit: 3}, {ID: 2, UUID: "b", SpeedLimit: 4},
		{ID: 3, UUID: "old"}, {ID: 4, UUID: "gone"},
	}
	right := []UserSpec{
		{ID: 2, UUID: "b", SpeedLimit: 9}, {ID: 1, UUID: "a", DeviceLimit: 0, SpeedLimit: 10},
		{ID: 3, UUID: "new"}, {ID: 5, UUID: "new grant"},
	}
	want := []UserSpec{{ID: 1, UUID: "a", DeviceLimit: 3, SpeedLimit: 10}, {ID: 2, UUID: "b", SpeedLimit: 4}}
	got := IntersectUsers(left, right)
	if !slices.Equal(got, want) {
		t.Fatalf("intersection = %v, want %v", got, want)
	}
	got[0].UUID = "different"
	if left[0].UUID != "a" || right[1].UUID != "a" || IntersectUsers(left, nil) == nil {
		t.Fatal("intersection must return its own list and preserve an explicit empty result")
	}
}
