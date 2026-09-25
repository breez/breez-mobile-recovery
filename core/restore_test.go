package core

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func zipOf(t *testing.T, entries ...[2]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(e[1]))
	}
	w.Close()
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBackupZipLayouts(t *testing.T) {
	// What the phone uploads to its cloud.
	files, err := unzipNodeFiles(zipOf(t, [2]string{"channel.db", "c"}, [2]string{"wallet.db", "w"}, [2]string{"breez.db", "b"}, [2]string{"channel.backup", "x"}))
	if err != nil || string(files["wallet.db"]) != "w" || len(files) != 3 {
		t.Errorf("cloud layout: %v %v", files, err)
	}
	// The phone's "Export DB Files": the first wallet is the backup copy,
	// the second the live file of the running app.
	files, err = unzipNodeFiles(zipOf(t, [2]string{"1_channel.db", "c"}, [2]string{"2_wallet.db", "backup copy"}, [2]string{"3_wallet.db", "live"}, [2]string{"4_breez.db", "b"}))
	if err != nil || string(files["wallet.db"]) != "backup copy" || string(files["channel.db"]) != "c" || string(files["breez.db"]) != "b" {
		t.Errorf("export layout: %v %v", files, err)
	}
	// Order inside the zip must not matter.
	files, err = unzipNodeFiles(zipOf(t, [2]string{"3_wallet.db", "live"}, [2]string{"4_breez.db", "b"}, [2]string{"2_wallet.db", "backup copy"}, [2]string{"1_channel.db", "c"}))
	if err != nil || string(files["wallet.db"]) != "backup copy" {
		t.Errorf("export layout, other order: %v %v", files, err)
	}
	// Anything else with one name twice is refused.
	for _, entries := range [][][2]string{
		{{"a/wallet.db", "1"}, {"b/wallet.db", "2"}, {"channel.db", "c"}, {"breez.db", "b"}},
		{{"wallet.db", "1"}, {"2_wallet.db", "2"}, {"channel.db", "c"}, {"breez.db", "b"}},
		{{"1_channel.db", "c"}, {"2_channel.db", "c"}, {"wallet.db", "w"}, {"breez.db", "b"}},
		{{"1_wallet.db", "1"}, {"2_wallet.db", "2"}, {"3_wallet.db", "3"}, {"channel.db", "c"}, {"breez.db", "b"}},
	} {
		if files, err := unzipNodeFiles(zipOf(t, entries...)); err == nil {
			t.Errorf("%v accepted: %v", entries, files)
		}
	}
}

// backupZip is a backup.zip holding the three node files.
func backupZip(t *testing.T, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range testNodeFiles(t, marker) {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(content)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// reply is an HTTP answer of a fake Google or Apple.
func reply(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

// Each restore keeps where it came from in the folder it placed, and the
// list of restored backups reads it back. Google and Apple are answered by
// fakes: nothing leaves the computer.
func TestEveryRestoreKeepsItsSource(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	ctx := context.Background()
	zipped := backupZip(t, "backup")

	// Google Drive: the snapshot folder of the node, the folder of its
	// newest files, the file, then the mark that it was restored.
	const googleNode = nodeA
	marked := false
	driveClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query().Get("q")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files") && strings.Contains(q, "name = '"+driveSnapshotPrefix+googleNode+"'"):
			return reply(200, []byte(`{"files":[{"id":"snap","name":"`+driveSnapshotPrefix+googleNode+`","appProperties":{"`+driveActiveFolderProp+`":"active"}}]}`)), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files") && strings.Contains(q, "'active' in parents"):
			return reply(200, []byte(fmt.Sprintf(`{"files":[{"id":"zip","name":"backup.zip","size":"%d"}]}`, len(zipped)))), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files/zip") && r.URL.Query().Get("alt") == "media":
			return reply(200, zipped), nil
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/files/snap"):
			marked = true
			return reply(200, []byte(`{"id":"snap"}`)), nil
		}
		return reply(404, []byte(`{"error":{"code":404,"message":"not faked"}}`)), nil
	})}
	t.Cleanup(func() { driveClient = nil })
	// A cached sign-in that is still valid: no browser, no token refresh.
	c.cfg.GoogleClientID = "test-client"
	tok, _ := json.Marshal(oauth2.Token{AccessToken: "access", TokenType: "Bearer", RefreshToken: "refresh", Expiry: time.Now().Add(24 * time.Hour)})
	if err := os.WriteFile(filepath.Join(root, tokenFileName), tok, 0600); err != nil {
		t.Fatal(err)
	}
	c.gsnaps = []Snapshot{{NodeID: googleNode}}
	if err := c.GoogleRestore(ctx, googleNode, "", false); err != nil {
		t.Fatal("Google Drive restore:", err)
	}
	if !marked {
		t.Error("the Drive backup was not marked as restored")
	}

	// iCloud: the listing, then the backup asset.
	const icloudNode = nodeB
	c.icloud = &icloudClient{apiToken: "api", session: "session", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/records/query"):
			return reply(200, []byte(`{"records":[{"recordName":"`+icloudNode+`","fields":{"backup":{"type":"ASSETID","value":{"downloadURL":"https://icloud.invalid/${f}"}}},"modified":{"timestamp":1758000000000}}]}`)), nil
		case r.Method == http.MethodGet && r.URL.Path == "/backup.zip":
			return reply(200, zipped), nil
		}
		return reply(404, nil), nil
	})}}
	if err := c.ICloudRestore(ctx, icloudNode, "", false); err != nil {
		t.Fatal("iCloud restore:", err)
	}

	// A backup file.
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	if err := os.WriteFile(zipPath, zipped, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.ZipRestore(zipPath, "", false); err != nil {
		t.Fatal("backup file restore:", err)
	}
	zipName, err := zipBackupName(zipPath)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{googleNode: SourceGoogle, icloudNode: SourceICloud, zipName: SourceFile}
	for name, source := range want {
		raw, err := os.ReadFile(filepath.Join(c.backupDir(name), sourceFile))
		if err != nil || string(raw) != source+"\n" {
			t.Errorf("%s: %s holds %q (%v), want %q", name, sourceFile, raw, err, source)
		}
	}
	got := map[string]string{}
	for _, b := range c.RestoredBackups() {
		got[b.Name] = b.Source
	}
	if len(got) != len(want) {
		t.Errorf("restored backups %v, want %v", got, want)
	}
	for name, source := range want {
		if got[name] != source {
			t.Errorf("%s: listed from %q, want %q", name, got[name], source)
		}
	}
}
