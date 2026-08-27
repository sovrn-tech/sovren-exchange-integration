package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A mutation route must be rejected without the configured bearer token and
// accepted with it — proving auth wraps the whole admin surface (PR #300 P1).
func TestAdminAuthEnforced(t *testing.T) {
	deps := testDeps(t)
	tok := "unit-test-bearer"
	deps.Config.Admin.AuthToken = tok
	mux := adminMux(deps)

	// No header → 401.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Wrong token → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/controls/pause", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Correct token → not 401 (reaches the handler).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// With no token configured, the mux serves without auth (loopback-bound dev
// default); config validation is what forbids a non-loopback bind here.
func TestAdminNoTokenServesOpen(t *testing.T) {
	mux := adminMux(testDeps(t))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestValidateAdminBinding(t *testing.T) {
	cases := []struct {
		name    string
		cfg     AdminConfig
		wantErr bool
	}{
		{"empty listen ok", AdminConfig{}, false},
		{"loopback ok", AdminConfig{Listen: "127.0.0.1:9465"}, false},
		{"loopback ipv6 ok", AdminConfig{Listen: "[::1]:9465"}, false},
		{"localhost ok", AdminConfig{Listen: "localhost:9465"}, false},
		{"wildcard without token refused", AdminConfig{Listen: "0.0.0.0:9465"}, true},
		{"routable ip without token refused", AdminConfig{Listen: "203.0.113.5:9465"}, true},
		{"empty-host wildcard without token refused", AdminConfig{Listen: ":9465"}, true},
		{"wildcard WITH token ok", AdminConfig{Listen: "0.0.0.0:9465", AuthToken: "t"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAdminBinding(tc.cfg)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
