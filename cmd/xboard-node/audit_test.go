package main

import "testing"

func TestStrictMemoryUnits(t *testing.T) {
	for input, want := range map[string]int64{"512MB": 512000000, "512MiB": 512 << 20, "12": 12, "0B": 0, "1gb": 1000000000} {
		got, e := parseMemLimit(input)
		if e != nil || got != want {
			t.Fatalf("%s => %d, %v", input, got, e)
		}
	}
	for _, input := range []string{"123x", "-1MB", "9223372036854775807GB", ""} {
		if _, e := parseMemLimit(input); e == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
