package config

import "testing"

func TestNormalizeFallbackTarget(t *testing.T) {
	for input, want := range map[string]string{
		"127.0.0.1:8080": "127.0.0.1:8080", " EXAMPLE.test:08443 ": "example.test:8443",
		"[2001:DB8::1]:443": "[2001:db8::1]:443", "localhost:1": "localhost:1", "site.test.:65535": "site.test.:65535",
		"": "", "80": "", "/run/site.sock": "", "https://site.test:443": "", "host:0": "", "host:65536": "", "host:+80": "", "host:000080": "",
		"999.0.0.1:80": "", "a..b:80": "", "-host:80": "", "host_:80": "", "::1:443": "", "[host]:80": "", "host:80/path": "", "host :80": "",
	} {
		if got := NormalizeFallbackTarget(input); got != want {
			t.Errorf("%q => %q, want %q", input, got, want)
		}
	}
}
