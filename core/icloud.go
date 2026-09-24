package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/breez/breez/backup"
)

// iOS stores the backup in CloudKit's private database of the app's
// container: one "BackupSnapshot" record per node (record name = node id)
// with the zip in the "backup" asset, or, for old records, three assets
// "walletdb", "channeldb" and "breezdb". See
// ios/Plugins/Breez/BreezLib/iCloudBackupProvider.swift in breezmobile.
//
// CloudKit Web Services expose the same database over REST. The API token
// identifies this tool to the container; the user still signs in with
// their Apple ID in the browser. Apple then redirects to the static
// callback page registered with the token (icloud-callback.html on the
// gh-pages branch of the breez library repo, breez/breez), which forwards
// the session token to the tool listening on localhost. Production tokens
// only accept https callbacks, which is why the page exists.
const (
	icloudContainer   = "iCloud.technology.breez.client"
	icloudEnvironment = "production"
	icloudCallbackURL = "https://breez.github.io/breez/icloud-callback.html"
	icloudListenAddr  = "127.0.0.1:53821"
	icloudTokenFile   = "icloud-session.json"
	cloudKitBase      = "https://api.apple-cloudkit.com/database/1/"
)

// icloudAPIToken is created in the CloudKit Console (Tokens & Keys, in the
// Production environment) with URL-redirect sign-in to icloudCallbackURL.
// It is a container identifier, not a secret.
var icloudAPIToken = "c13eed55a5a618a6788831fc0b193658b39b46659491e7e6f2fd7a03467066f6"

type icloudClient struct {
	apiToken    string
	session     string // ckWebAuthToken
	sessionPath string // where the session is cached; empty: not cached
	http        *http.Client
}

func (c *icloudClient) url(database, subpath string) string {
	u := cloudKitBase + icloudContainer + "/" + icloudEnvironment + "/" + database + "/" + subpath
	q := url.Values{"ckAPIToken": {c.apiToken}}
	if c.session != "" {
		q.Set("ckWebAuthToken", c.session)
	}
	return u + "?" + q.Encode()
}

func (c *icloudClient) post(database, subpath string, body interface{}, dst interface{}) (int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", c.url(database, subpath), bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, withoutURL(err)
	}
	defer res.Body.Close()
	c.rotate(res)
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, fmt.Errorf("cloudkit %s: read the reply: %w", subpath, err)
	}
	if res.StatusCode >= 400 {
		var e struct {
			ServerErrorCode string `json:"serverErrorCode"`
			Reason          string `json:"reason"`
		}
		_ = json.Unmarshal(data, &e)
		return res.StatusCode, fmt.Errorf("cloudkit %s: %s %s", subpath, e.ServerErrorCode, e.Reason)
	}
	if dst != nil {
		// A reply that cannot be read is an error, never "no backups".
		if err := json.Unmarshal(data, dst); err != nil {
			return res.StatusCode, fmt.Errorf("cloudkit %s: unreadable reply: %w", subpath, err)
		}
	}
	return res.StatusCode, nil
}

// rotate takes the session CloudKit sends back with a reply. Apple's docs
// make a ckWebAuthToken good for one round trip, the reply carrying the
// token for the next request; CloudKit JS reads it from these two headers
// (getCKSession in cdn.apple-cloudkit.com/ck/2/cloudkit.js) and saves it.
func (c *icloudClient) rotate(res *http.Response) {
	if c.session == "" {
		return
	}
	next := res.Header.Get("X-Apple-CloudKit-Web-Auth-Token")
	if next == "" {
		next = res.Header.Get("X-Apple-CloudKit-Session")
	}
	if next == "" || next == c.session {
		return
	}
	c.session = next
	c.saveSession()
}

func (c *icloudClient) saveSession() {
	if c.sessionPath == "" {
		return
	}
	if data, err := json.Marshal(struct{ Session string }{c.session}); err == nil {
		_ = os.WriteFile(c.sessionPath, data, 0600)
	}
}

// withoutURL strips the request URL from a network error: it carries the
// session token in its query, and Go prints the whole URL, which would put
// the token into the log the user saves for support.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s iCloud: %w", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}

// icloudSignIn returns a client with a valid user session, reusing a cached
// one when it still works and running the browser sign-in otherwise.
func (c *Core) icloudSignIn(ctx context.Context) (*icloudClient, error) {
	sessionPath := filepath.Join(c.cfg.WorkDir, icloudTokenFile)
	client := &icloudClient{apiToken: c.cfg.ICloudAPIToken, sessionPath: sessionPath, http: &http.Client{Timeout: 60 * time.Second}}
	if client.apiToken == "" {
		return nil, errors.New("no CloudKit API token configured")
	}
	if err := os.MkdirAll(c.cfg.WorkDir, 0700); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(sessionPath); err == nil {
		var cached struct{ Session string }
		if json.Unmarshal(data, &cached) == nil && cached.Session != "" {
			client.session = cached.Session
			if ok, _ := client.sessionValid(); ok {
				return client, nil
			}
			client.session = ""
			os.Remove(sessionPath)
		}
	}

	// Ask for the current user without a session: CloudKit answers 421 with
	// the Apple sign-in URL.
	var auth struct {
		RedirectURL string `json:"redirectURL"`
	}
	req, err := http.NewRequest("GET", client.url("public", "users/current"), nil)
	if err != nil {
		return nil, err
	}
	res, err := client.http.Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	_ = json.Unmarshal(data, &auth)
	if auth.RedirectURL == "" {
		return nil, fmt.Errorf("cloudkit did not offer a sign-in URL (HTTP %d): %s", res.StatusCode, strings.TrimSpace(string(data)))
	}

	session, err := c.icloudLoopback(ctx, auth.RedirectURL)
	if err != nil {
		return nil, err
	}
	client.session = session
	if ok, err := client.sessionValid(); !ok {
		return nil, fmt.Errorf("apple sign-in did not yield a usable session: %v", err)
	}
	client.saveSession()
	return client, nil
}

// ForgetICloud removes the cached Apple session so the next attempt asks
// again in the browser.
func (c *Core) ForgetICloud() {
	if c.icloud != nil {
		c.icloud.sessionPath = "" // a rotated session must not write it back
	}
	os.Remove(filepath.Join(c.cfg.WorkDir, icloudTokenFile))
}

func (c *icloudClient) sessionValid() (bool, error) {
	req, err := http.NewRequest("GET", c.url("private", "users/current"), nil)
	if err != nil {
		return false, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return false, withoutURL(err)
	}
	defer res.Body.Close()
	c.rotate(res)
	return res.StatusCode == 200, fmt.Errorf("HTTP %d", res.StatusCode)
}

// icloudLoopback serves the localhost endpoint the callback page forwards
// to. Apple may put the session token in the query (ckSession) or in the
// URL fragment, so the page also forwards fragment parameters as a query.
func (c *Core) icloudLoopback(ctx context.Context, signInURL string) (string, error) {
	ln, err := net.Listen("tcp", icloudListenAddr)
	if err != nil {
		return "", fmt.Errorf("cannot listen on %s for the Apple sign-in callback: %w", icloudListenAddr, err)
	}
	defer ln.Close()

	tokenCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/icloud" {
			http.NotFound(w, r)
			return
		}
		if v := callbackToken(r.URL.Query()); v != "" {
			// Send the browser on to the hosted page so the token
			// leaves the address bar, then hand the token over.
			http.Redirect(w, r, SignedInPageURL, http.StatusFound)
			select {
			case tokenCh <- v:
			default:
			}
			return
		}
		// No token in the query: it may be in the fragment, which only the
		// browser can see. Forward it as a query string.
		fmt.Fprint(w, `<html><body><script>
var h=location.hash.replace(/^#/,'');
if(h){location.replace(location.pathname+'?'+h);}else{document.write('No sign-in token received.');}
</script></body></html>`)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c.progressf("Waiting for the Apple ID sign-in in your browser...")
	c.rep.SignIn("icloud", signInURL)
	openBrowser(signInURL)

	select {
	case tok := <-tokenCh:
		return tok, nil
	case <-time.After(10 * time.Minute):
		return "", errors.New("timed out waiting for the Apple sign-in")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// callbackToken reads the session token from the callback query. The token
// is base64 and never holds a space, but a '+' that Apple leaves unencoded
// becomes one when the callback page or this handler decodes the query
// (URLSearchParams and url.Values both read '+' as a space): put it back.
func callbackToken(q url.Values) string {
	for _, k := range []string{"ckSession", "ckWebAuthToken"} {
		if v := q.Get(k); v != "" {
			return strings.ReplaceAll(v, " ", "+")
		}
	}
	return ""
}

type ckRecord struct {
	RecordName string `json:"recordName"`
	Fields     map[string]struct {
		Value interface{} `json:"value"`
		Type  string      `json:"type"`
	} `json:"fields"`
	Modified struct {
		Timestamp int64 `json:"timestamp"`
	} `json:"modified"`
}

func (r ckRecord) str(field string) string {
	if f, ok := r.Fields[field]; ok {
		if s, ok := f.Value.(string); ok {
			return s
		}
	}
	return ""
}

func (r ckRecord) asset(field string) (downloadURL string, ok bool) {
	f, ok := r.Fields[field]
	if !ok {
		return "", false
	}
	m, ok := f.Value.(map[string]interface{})
	if !ok {
		return "", false
	}
	u, _ := m["downloadURL"].(string)
	return u, u != ""
}

// snapshots lists the BackupSnapshot records in the user's private database.
func (c *icloudClient) snapshots() ([]backup.SnapshotInfo, map[string]ckRecord, error) {
	// CloudKit answers in pages: a reply may carry a continuation marker,
	// even with few or no records in it.
	var all []ckRecord
	marker := ""
	for page := 0; ; page++ {
		var res struct {
			Records            []ckRecord `json:"records"`
			ContinuationMarker string     `json:"continuationMarker"`
		}
		body := map[string]interface{}{
			"query":  map[string]interface{}{"recordType": "BackupSnapshot"},
			"zoneID": map[string]string{"zoneName": "_defaultZone"},
		}
		if marker != "" {
			body["continuationMarker"] = marker
		}
		if _, err := c.post("private", "records/query", body, &res); err != nil {
			return nil, nil, err
		}
		all = append(all, res.Records...)
		if marker = res.ContinuationMarker; marker == "" {
			break
		}
		if page > 1000 {
			return nil, nil, errors.New("iCloud keeps answering with more pages of backups")
		}
	}
	var snaps []backup.SnapshotInfo
	records := map[string]ckRecord{}
	for _, r := range all {
		encType := r.str("backupEncryptionType")
		modified := time.UnixMilli(r.Modified.Timestamp)
		if ts := r.str("timestamp"); ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				modified = t
			}
		}
		snaps = append(snaps, backup.SnapshotInfo{
			NodeID:         r.RecordName,
			BackupID:       r.str("backupID"),
			Encrypted:      encType != "",
			EncryptionType: encType,
			ModifiedTime:   modified,
		})
		records[r.RecordName] = r
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ModifiedTime.After(snaps[j].ModifiedTime) })
	if len(snaps) == 0 {
		return nil, nil, errors.New("no Breez backups found in this Apple ID's iCloud")
	}
	return snaps, records, nil
}

// download fetches the backup files of a record into memory, by file name:
// either a single backup.zip or the three legacy database files.
func (c *icloudClient) download(r ckRecord) (map[string][]byte, error) {
	fetch := func(field, name string) ([]byte, error) {
		u, ok := r.asset(field)
		if !ok {
			return nil, fmt.Errorf("record has no %s asset", field)
		}
		u = strings.ReplaceAll(u, "${f}", name)
		res, err := c.http.Get(u)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, withoutURL(err))
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("download %s: HTTP %d", name, res.StatusCode)
		}
		content, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		if res.ContentLength > 0 && int64(len(content)) != res.ContentLength {
			return nil, fmt.Errorf("download %s: got %d of %d bytes", name, len(content), res.ContentLength)
		}
		return content, nil
	}

	out := map[string][]byte{}
	if _, ok := r.asset("backup"); ok {
		content, err := fetch("backup", "backup.zip")
		if err != nil {
			return nil, err
		}
		out["backup.zip"] = content
		return out, nil
	}
	for _, f := range []struct{ field, name string }{{"walletdb", "wallet.db"}, {"channeldb", "channel.db"}, {"breezdb", "breez.db"}} {
		content, err := fetch(f.field, f.name)
		if err != nil {
			return nil, err
		}
		out[f.name] = content
	}
	return out, nil
}
