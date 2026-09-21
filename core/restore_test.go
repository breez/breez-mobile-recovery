package core

import (
	"archive/zip"
	"bytes"
	"testing"
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
