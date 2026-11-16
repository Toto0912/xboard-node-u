package porthopping

import (
	"errors"
	"testing"
)

func TestRangeValidationAndAddressScope(t *testing.T) {
	for _, invalid := range []string{"1", "65000-60000", "0-10", "60000-65536", "1-2,3-4", "1-1", "1-2;true", "999999999999-65535"} {
		if _, err := NewSpec("::", invalid, 60000); err == nil {
			t.Fatalf("accepted invalid range %q", invalid)
		}
	}
	if _, err := NewSpec("::", "60000-65000", 60001); err == nil {
		t.Fatal("accepted mismatching listener")
	}
	if _, err := NewSpec("example.test", "60000-65000", 60000); err == nil {
		t.Fatal("accepted unresolved bind address")
	}
	a, err := NewSpec("::", "60000-65000", 60000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSpec("127.0.0.1", "61000-62000", 61000)
	c, _ := NewSpec("127.0.0.2", "61000-62000", 61000)
	if !overlap(a, b) || overlap(b, c) {
		t.Fatal("incorrect per-address conflict detection")
	}
}

func TestActivationFailureRestoresOldKernel(t *testing.T) {
	restored := false
	err := activateOnly(func() error { return errors.New("new kernel failed") }, func() error { restored = true; return nil })
	if err == nil || !restored {
		t.Fatal("failed kernel was not rolled back")
	}
}
