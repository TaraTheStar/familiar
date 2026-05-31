// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMountBrand verifies the WS3/WS6 routing: the /grimoire/ brand resolves the
// OTA endpoint (slash and bare) and the WS session endpoint to the right
// handler, and the retired /xiaozhi/ brand is no longer served (404 — the device
// is reflashed to /grimoire/ before coming back online, and v1 is gone).
func TestMountBrand(t *testing.T) {
	const (
		hitOTA     = "ota"
		hitSession = "session"
	)
	var last string
	mark := func(tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { last = tag }
	}

	mux := http.NewServeMux()
	mountBrand(mux, "grimoire", mark(hitOTA), mark(hitSession))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A client that refuses to follow redirects, so a stray ServeMux subtree
	// 307 (the bug the bare /ota mount prevents) surfaces as a failure rather
	// than being silently followed.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resolves := []struct {
		path string
		want string
	}{
		{"/grimoire/ota/", hitOTA},
		{"/grimoire/ota", hitOTA}, // bare form must not 307-redirect away
		{"/grimoire/", hitSession},
	}
	for _, c := range resolves {
		last = ""
		resp, err := client.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d (want 200; a 3xx means the route redirected instead of resolving)", c.path, resp.StatusCode)
			continue
		}
		if last != c.want {
			t.Errorf("GET %s: hit %q handler, want %q", c.path, last, c.want)
		}
	}

	// The retired /xiaozhi/ brand is no longer served → 404.
	for _, gone := range []string{"/xiaozhi/ota/", "/xiaozhi/ota", "/xiaozhi/"} {
		resp, err := client.Get(srv.URL + gone)
		if err != nil {
			t.Fatalf("GET %s: %v", gone, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 (legacy brand retired)", gone, resp.StatusCode)
		}
	}
}
