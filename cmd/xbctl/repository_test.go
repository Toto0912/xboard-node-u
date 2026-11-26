package main

import "testing"

func TestReleaseRepository(t *testing.T) {
	previous := releaseRepository
	t.Cleanup(func() { releaseRepository = previous })
	releaseRepository = "builder/node"
	for _, tc := range []struct {
		name, override, want string
		wantError            bool
	}{
		{"embedded", "", "https://github.com/builder/node/releases", false},
		{"override", "my-account/my-node", "https://github.com/my-account/my-node/releases", false},
		{"url rejected", "https://github.com/other/node", "", true},
		{"traversal rejected", "owner/..", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XBOARD_NODE_REPOSITORY", tc.override)
			got, err := releaseBaseURL()
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.wantError)
			}
		})
	}
	t.Run("unconfigured local build", func(t *testing.T) {
		releaseRepository = ""
		t.Setenv("XBOARD_NODE_REPOSITORY", "")
		if _, err := releaseBaseURL(); err == nil {
			t.Fatal("unconfigured build must not fall back to upstream releases")
		}
	})
}
