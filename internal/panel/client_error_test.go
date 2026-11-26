package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

type failingPanelTransport func(*http.Request) error

func (f failingPanelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, f(r)
}

func assertSafeGETError(t *testing.T, err error, path, token, reason string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a request error")
	}
	message := fmt.Sprintf("error=%v", err)
	for _, want := range []string{"GET", path, reason} {
		if !strings.Contains(message, want) {
			t.Errorf("error is missing %q", want)
		}
	}
	for _, private := range []string{token, url.QueryEscape(token), "?", "token=", "machine_id=", "node_id="} {
		if strings.Contains(message, private) {
			t.Fatal("error contains request credentials or query parameters")
		}
	}
}

func TestGETTransportErrorsOmitCredentialsAndKeepCause(t *testing.T) {
	for _, machineID := range []int{0, 5} {
		for _, users := range []bool{false, true} {
			t.Run(fmt.Sprintf("machine%d/users%t", machineID, users), func(t *testing.T) {
				token := fmt.Sprintf("fixture+token/%d", machineID)
				c := NewClient(config.PanelConfig{URL: "http://panel.invalid", Token: token, NodeID: 9, MachineID: machineID})
				c.httpClient.Transport = failingPanelTransport(func(r *http.Request) error {
					if r.URL.Query().Get("token") != token || r.URL.Query().Get("node_id") != "9" {
						t.Fatal("expected normal GET authentication")
					}
					return context.DeadlineExceeded
				})
				path := c.configPath()
				var err error
				if users {
					path = c.userPath()
					_, err = c.GetUsers()
				} else {
					_, err = c.GetConfig()
				}
				assertSafeGETError(t, err, path, token, context.DeadlineExceeded.Error())
				var urlErr *url.Error
				if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &urlErr) || !urlErr.Timeout() || urlErr.URL != path {
					t.Fatal("expected a sanitized timeout wrapping the deadline cause")
				}
			})
		}
	}
}

func TestGETErrorsOmitAuthenticationFromReflectedResponses(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw := r.URL.RequestURI()
				if status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/%zz"+raw)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, raw)
			}))
			defer server.Close()
			c := NewClient(config.PanelConfig{URL: server.URL, Token: "echo+fixture/1", NodeID: 2})
			reason := "failed to parse Location"
			if status == http.StatusBadGateway {
				reason = http.StatusText(status)
			}
			_, err := c.GetConfig()
			assertSafeGETError(t, err, c.configPath(), c.token, reason)
			_, err = c.GetUsers()
			assertSafeGETError(t, err, c.userPath(), c.token, reason)
		})
	}
}

func TestInvalidGETRequestURLIsSafeToLog(t *testing.T) {
	c := NewClient(config.PanelConfig{URL: "http://panel.invalid/%zz", Token: "parse+fixture/1", NodeID: 2})
	_, err := c.GetUsers()
	assertSafeGETError(t, err, c.userPath(), c.token, "invalid URL escape")
}
