package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With --gateway-key every route but the dashboard page wants the key, in
// either of the headers clients send one in.
func TestGatewayKey(t *testing.T) {
	const key = "sekret-key"
	h := testNode(t, t.Context(), Config{GatewayKey: key}).Gateway()
	send := func(method, target string, headers ...string) *httptest.ResponseRecorder {
		r := gwReq(method, target, strings.NewReader("{}"))
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	routes := []struct{ method, path string }{
		{"GET", "/status"}, {"GET", "/v1/models"}, {"GET", "/queue"}, {"GET", "/network"}, {"GET", "/logs"},
		{"GET", "/audits"}, {"GET", "/account"}, {"GET", "/members"}, {"GET", "/members/me"}, {"GET", "/credits"},
		{"POST", "/v1/chat/completions"}, {"POST", "/v1/completions"}, {"POST", "/v1/messages"},
		{"POST", "/account/message"}, {"POST", "/account/link"}, {"POST", "/members"},
		{"HEAD", "/"}, {"POST", "/"}, {"GET", "/nothing-here"},
	}
	for _, rt := range routes {
		for _, headers := range [][]string{
			nil,
			{"Authorization", "Bearer wrong"},
			{"Authorization", "Bearer " + key + "x"},
			{"Authorization", key},
			{"Authorization", "Basic " + key},
			{"x-api-key", "wrong"},
			{"x-api-key", ""},
		} {
			if w := send(rt.method, rt.path, headers...); w.Code != 401 {
				t.Errorf("%s %s with %v: %d, want 401", rt.method, rt.path, headers, w.Code)
			} else if !strings.Contains(w.Body.String(), `"error"`) {
				t.Errorf("%s %s: the 401 carries no error body: %q", rt.method, rt.path, w.Body)
			}
		}
	}
	for _, headers := range [][]string{
		{"Authorization", "Bearer " + key},
		{"Authorization", "bearer " + key},
		{"x-api-key", key},
		{"Authorization", "Bearer wrong", "x-api-key", key},
		{"Authorization", "Bearer " + key, "x-api-key", "wrong"},
	} {
		if w := send("GET", "/status", headers...); w.Code != 200 {
			t.Errorf("GET /status with %v: %d", headers, w.Code)
		}
	}
	// The page is what a browser loads before it has the key, and it holds no data.
	if w := send("GET", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("GET /: %d", w.Code)
	}
}

// The key adds to the Host and Origin checks and replaces neither.
func TestGatewayKeyKeepsHostAndOriginChecks(t *testing.T) {
	const key = "sekret-key"
	h := testNode(t, t.Context(), Config{GatewayKey: key}).Gateway()
	for _, path := range []string{"/", "/status"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "evil.example:7778"
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("GET %s under a non-local Host, with the key: %d, want 403", path, w.Code)
		}
	}
	r := gwReq("POST", "/members", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Origin", "http://other.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("cross-origin POST with the key: %d, want 403", w.Code)
	}
}

// Without a key nothing changes, and an IP literal Host still passes.
func TestGatewayWithoutKeyStaysOpen(t *testing.T) {
	h := testNode(t, t.Context(), Config{}).Gateway()
	for _, host := range []string{"127.0.0.1:7778", "192.168.1.5:7778", "[::1]:7778", "localhost:7778"} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("Host %s: %d", host, w.Code)
		}
	}
}
