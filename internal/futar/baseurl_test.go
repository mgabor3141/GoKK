package futar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBaseURLDefaultsToTheAPI(t *testing.T) {
	t.Setenv(BaseURLEnv, "")
	if got := BaseURL(); got != DefaultBaseURL {
		t.Errorf("BaseURL() = %q, want %q", got, DefaultBaseURL)
	}
	if got := New("key").BaseURL; got != DefaultBaseURL {
		t.Errorf("New().BaseURL = %q, want %q", got, DefaultBaseURL)
	}
}

func TestBaseURLOverrideIsTrimmed(t *testing.T) {
	want := "http://bkk-proxy:8080/v1/otp/api/where"
	t.Setenv(BaseURLEnv, "  "+want+"/  ")
	if got := BaseURL(); got != want {
		t.Errorf("BaseURL() = %q, want %q (trailing slash would join as //endpoint)", got, want)
	}
	if got := New("key").BaseURL; got != want {
		t.Errorf("New().BaseURL = %q, want %q", got, want)
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, good := range []string{DefaultBaseURL, "http://bkk-proxy:8080/v1/otp/api/where"} {
		if err := validateBaseURL(good); err != nil {
			t.Errorf("validateBaseURL(%q) = %v, want nil", good, err)
		}
	}
	for _, bad := range []string{"", "file:///etc/passwd", "ftp://host/x", "http://", "/otp/api/where"} {
		err := validateBaseURL(bad)
		if err == nil {
			t.Errorf("validateBaseURL(%q) = nil, want an error", bad)
			continue
		}
		// The message has to name the variable: an operator staring at a broken
		// deployment should not have to guess which knob to look at.
		if !strings.Contains(err.Error(), BaseURLEnv) {
			t.Errorf("validateBaseURL(%q) error should name %s, got: %v", bad, BaseURLEnv, err)
		}
	}
}

// The point of the override: a proxy that speaks the same API answers exactly the
// requests the real one does, credential included.
func TestOverriddenBaseReceivesTheRequest(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.URL.Query().Get("key")
		_, _ = w.Write([]byte(`{"code":200,"data":{}}`))
	}))
	defer srv.Close()

	c := &Client{Key: "downstream-token", BaseURL: srv.URL + "/v1/otp/api/where", HTTP: newHTTPClient()}
	if _, err := c.Arrivals(context.Background(), []string{"BKK_F05175"}, nil, 0); err != nil {
		t.Fatalf("Arrivals: %v", err)
	}
	if want := "/v1/otp/api/where/arrivals-and-departures-for-stop"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotKey != "downstream-token" {
		t.Errorf("key = %q, want the configured credential sent to the configured base", gotKey)
	}
}

// The key travels as a query parameter and net/http replays the full URL when it
// follows a redirect, so a redirect to another host is a credential leak unless the
// client says otherwise. This holds for the default base URL too, not only for an
// override; it is why the guard lives in the client and not in the env handling.
func TestCrossHostRedirectDoesNotLeakTheKey(t *testing.T) {
	var mu sync.Mutex
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHit = true
		mu.Unlock()
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/arrivals-and-departures-for-stop", http.StatusFound)
	}))
	defer origin.Close()

	c := &Client{Key: "secret", BaseURL: origin.URL + "/otp/api/where", HTTP: newHTTPClient()}
	_, err := c.Arrivals(context.Background(), []string{"BKK_F05175"}, nil, 0)
	if err == nil {
		t.Fatal("want an error when the API redirects to another host")
	}
	if !strings.Contains(err.Error(), "request URL") {
		t.Errorf("error should explain that the key is at stake, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if targetHit {
		t.Error("the redirect target was contacted; the key left the machine")
	}
}

// A same-host redirect is the only legitimate one here (an http -> https upgrade, a
// path rewrite), and refusing it would break the client for a normal operation.
// A connection failure is the normal way this code path is exercised, and Go formats
// a *url.Error with the entire URL in it - which is the user's API key.
func TestNetworkErrorDoesNotPrintTheKey(t *testing.T) {
	c := &Client{Key: "proxy-token-hunter2", BaseURL: "http://127.0.0.1:1/v1/otp/api/where", HTTP: newHTTPClient()}
	_, err := c.Arrivals(context.Background(), []string{"BKK_F05175"}, nil, 0)
	if err == nil {
		t.Fatal("want a network error from a port nothing listens on")
	}
	if strings.Contains(err.Error(), "proxy-token-hunter2") {
		t.Errorf("the credential reached the error message: %v", err)
	}
	if !strings.Contains(err.Error(), "key=***") {
		t.Errorf("want the redacted placeholder, got: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("want the host that failed, got: %v", err)
	}
}

// The same reasoning as BKK_BASE_URL itself: an error that names the wrong host sends
// the operator to the wrong machine.
func TestErrorMessageNamesTheConfiguredHost(t *testing.T) {
	c := &Client{Key: "k", BaseURL: "http://bkk-proxy.invalid:8080/v1/otp/api/where", HTTP: newHTTPClient()}
	_, err := c.Arrivals(context.Background(), []string{"BKK_F05175"}, nil, 0)
	if err == nil {
		t.Fatal("want a network error")
	}
	if !strings.Contains(err.Error(), "bkk-proxy.invalid:8080") {
		t.Errorf("want the configured host in the message, got: %v", err)
	}
	if strings.Contains(err.Error(), DefaultBaseURL) {
		t.Errorf("the message still names the default API, got: %v", err)
	}
}

func TestBaseURLWithoutSchemeIsExplained(t *testing.T) {
	// The realistic mistake: a bare host:port. url.Parse reads "bkk-proxy" as a scheme,
	// which is true but useless to whoever is fixing a deployment at 23:00.
	err := validateBaseURL("bkk-proxy:8080/v1/otp/api/where")
	if err == nil {
		t.Fatal("want an error for a URL with no scheme")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("want the message to say what is missing, got: %v", err)
	}
}

func TestSameHostRedirectIsFollowed(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/otp/api/where/arrivals-and-departures-for-stop" {
			http.Redirect(w, r, "/v1/otp/api/where/arrivals-and-departures-for-stop", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{}}`))
	}))
	defer srv.Close()

	c := &Client{Key: "k", BaseURL: srv.URL + "/otp/api/where", HTTP: newHTTPClient()}
	if _, err := c.Arrivals(context.Background(), []string{"BKK_F05175"}, nil, time.Duration(0)); err != nil {
		t.Fatalf("Arrivals: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("requests = %v, want the redirect to be followed once to the same host", paths)
	}
}
