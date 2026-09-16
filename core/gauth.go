package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/oauth2"
)

// Google Drive stores the mobile backup in the hidden appDataFolder, which is
// scoped to the Google Cloud project the app was registered in. Any OAuth
// client created inside that same project sees the same folder, so this tool
// needs a "Desktop app" OAuth client from the Breez project.
const (
	driveAppDataScope = "https://www.googleapis.com/auth/drive.appdata"
	tokenFileName     = "gdrive-token.json"
)

var googleEndpoint = oauth2.Endpoint{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
}

// googleAuth implements the breez AuthService/AppServices sign-in contract:
// SignIn returns a fresh access token. The oauth2 token source refreshes it
// transparently using the cached refresh token.
type googleAuth struct {
	src oauth2.TokenSource
}

func (g *googleAuth) SignIn() (string, error) {
	tok, err := g.src.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (c *Core) googleOAuthConfig() (*oauth2.Config, error) {
	if c.cfg.GoogleClientID == "" {
		return nil, errors.New("no Google OAuth client configured: set BREEZ_GOOGLE_CLIENT_ID and BREEZ_GOOGLE_CLIENT_SECRET (a Desktop app client from the Breez Google Cloud project)")
	}
	return &oauth2.Config{
		ClientID:     c.cfg.GoogleClientID,
		ClientSecret: c.cfg.GoogleClientSecret,
		Endpoint:     googleEndpoint,
		Scopes:       []string{driveAppDataScope},
	}, nil
}

// newGoogleAuth returns a token source, reusing a cached refresh token from
// the work dir when present and running the browser sign-in otherwise.
func (c *Core) newGoogleAuth(ctx context.Context) (*googleAuth, error) {
	cfg, err := c.googleOAuthConfig()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.cfg.WorkDir, 0700); err != nil {
		return nil, err
	}
	tokenPath := filepath.Join(c.cfg.WorkDir, tokenFileName)

	var tok *oauth2.Token
	if data, err := os.ReadFile(tokenPath); err == nil {
		var cached oauth2.Token
		if json.Unmarshal(data, &cached) == nil && cached.RefreshToken != "" {
			tok = &cached
		}
	}
	if tok == nil {
		tok, err = c.loopbackSignIn(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if data, err := json.Marshal(tok); err == nil {
			_ = os.WriteFile(tokenPath, data, 0600)
		}
	}

	src := oauth2.ReuseTokenSource(tok, &savingSource{
		inner: cfg.TokenSource(ctx, tok),
		path:  tokenPath,
	})
	// Fail early with a clear message if the cached token is dead.
	if _, err := src.Token(); err != nil {
		os.Remove(tokenPath)
		return nil, fmt.Errorf("google sign-in failed (cached token removed, try again): %w", err)
	}
	return &googleAuth{src: src}, nil
}

// ForgetGoogle removes the cached Google sign-in so the next attempt asks
// again in the browser.
func (c *Core) ForgetGoogle() {
	os.Remove(filepath.Join(c.cfg.WorkDir, tokenFileName))
}

// savingSource persists every refreshed token so later runs skip the browser.
type savingSource struct {
	inner oauth2.TokenSource
	path  string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.inner.Token()
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(s.path, data, 0600)
	}
	return tok, nil
}

// loopbackSignIn runs the OAuth authorization code flow with PKCE, receiving
// the redirect on a random localhost port.
func (c *Core) loopbackSignIn(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://%s/", ln.Addr().String())

	state := randomString(16)
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("oauth state mismatch")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("google sign-in denied: %s", e)
			return
		}
		fmt.Fprint(w, signedInPage)
		codeCh <- q.Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c.progressf("Waiting for the Google sign-in in your browser...")
	c.rep.SignIn("google", authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-time.After(10 * time.Minute):
		return nil, errors.New("timed out waiting for the Google sign-in")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("google did not return a refresh token; remove the app under myaccount.google.com/permissions and sign in again")
	}
	return tok, nil
}

const signedInPage = `<!doctype html><html><head><meta charset="utf-8"><title>Breez Recovery</title></head>
<body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#f4f7fb;color:#0c1e3c;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
<div style="background:#fff;padding:40px 48px;border-radius:16px;box-shadow:0 8px 30px rgba(12,30,60,.08);text-align:center">
<h2 style="margin:0 0 8px">Signed in</h2><p style="margin:0;color:#5b6b85">You can close this tab and return to Breez Recovery.</p></div></body></html>`

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// OpenBrowser opens url in the system browser.
func OpenBrowser(url string) { openBrowser(url) }

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
