package main

import "testing"

func TestReleaseRepository(t *testing.T) {
	previous := releaseRepository
	t.Cleanup(func() { releaseRepository = previous })
	releaseRepository = "builder/node"
	for _, tc := range []struct {
		name, override, version, want string
		wantError                     bool
	}{
		{"embedded", "", "latest", "https://github.com/builder/node/releases/latest/download/xbctl-linux-amd64", false},
		{"override", "my-account/my-node", "v0.1.0", "https://github.com/my-account/my-node/releases/download/v0.1.0/xbctl-linux-amd64", false},
		{"url rejected", "https://github.com/other/node", "latest", "", true},
		{"traversal rejected", "owner/..", "latest", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XBOARD_NODE_REPOSITORY", tc.override)
			got, err := resolveDownloadURL("xbctl-linux-amd64", tc.version)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.wantError)
			}
		})
	}
	t.Run("unconfigured local build", func(t *testing.T) {
		releaseRepository = ""
		t.Setenv("XBOARD_NODE_REPOSITORY", "")
		if _, err := resolveDownloadURL("xbctl-linux-amd64", "latest"); err == nil {
			t.Fatal("unconfigured build must not fall back to upstream releases")
		}
	})
}
