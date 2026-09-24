package core

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The token Apple puts in the callback can hold '+', '/' and '='. It
// reaches the tool either straight from Apple's redirect ('+' maybe left
// raw) or re-encoded by the callback page after URLSearchParams turned a
// raw '+' into a space (encodeURIComponent then sends %20).
func TestCallbackTokenKeepsPlus(t *testing.T) {
	want := "147__54__ab+cd/ef=="
	for _, raw := range []string{
		"ckWebAuthToken=147__54__ab+cd/ef==",
		"ckWebAuthToken=147__54__ab%2Bcd%2Fef%3D%3D",
		"ckWebAuthToken=147__54__ab%20cd%2Fef%3D%3D",
		"ckSession=147__54__ab+cd/ef==",
	} {
		q, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := callbackToken(q); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	if got := callbackToken(url.Values{}); got != "" {
		t.Errorf("no token: got %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Each request must carry the session the previous reply handed back, from
// either header CloudKit JS reads, and the cache must hold the latest one
// until the user forgets it.
func TestICloudSessionRotates(t *testing.T) {
	replies := []string{"tok2+/=", "tok3", "", "tok4"}
	headers := []string{"X-Apple-CloudKit-Web-Auth-Token", "X-Apple-CloudKit-Session", "", "X-Apple-CloudKit-Web-Auth-Token"}
	var sent []string
	c := testCore(t, t.TempDir())
	client := &icloudClient{
		apiToken:    "api",
		session:     "tok1",
		sessionPath: filepath.Join(c.cfg.WorkDir, icloudTokenFile),
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			i := len(sent)
			sent = append(sent, r.URL.Query().Get("ckWebAuthToken"))
			res := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"records":[]}`))}
			if replies[i] != "" {
				res.Header.Set(headers[i], replies[i])
			}
			return res, nil
		})},
	}

	if ok, _ := client.sessionValid(); !ok {
		t.Fatal("session not valid")
	}
	var dst struct{}
	for i := 0; i < 2; i++ {
		if _, err := client.post("private", "records/query", map[string]string{}, &dst); err != nil {
			t.Fatal(err)
		}
	}

	if want := []string{"tok1", "tok2+/=", "tok3"}; strings.Join(sent, " ") != strings.Join(want, " ") {
		t.Errorf("sent %q, want %q", sent, want)
	}
	data, err := os.ReadFile(client.sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"Session":"tok3"}` {
		t.Errorf("cached %s", data)
	}

	c.icloud = client
	c.ForgetICloud()
	if _, err := client.post("private", "records/query", map[string]string{}, &dst); err != nil {
		t.Fatal(err)
	}
	if client.session != "tok4" {
		t.Errorf("session %q after forget, want tok4", client.session)
	}
	if _, err := os.Stat(filepath.Join(c.cfg.WorkDir, icloudTokenFile)); !os.IsNotExist(err) {
		t.Errorf("forgotten session written back: %v", err)
	}
}

// A cached session is spent by the check that validates it: the rotated
// one must be what the cache holds afterwards.
func TestICloudSignInCachesRotatedSession(t *testing.T) {
	c := testCore(t, t.TempDir())
	path := filepath.Join(c.cfg.WorkDir, icloudTokenFile)
	if err := os.WriteFile(path, []byte(`{"Session":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var sent string
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.URL.Query().Get("ckWebAuthToken")
		res := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}
		res.Header.Set("X-Apple-CloudKit-Web-Auth-Token", "new")
		return res, nil
	})

	client, err := c.icloudSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if sent != "old" || client.session != "new" || string(data) != `{"Session":"new"}` {
		t.Errorf("sent %q, session %q, cached %s", sent, client.session, data)
	}
}
