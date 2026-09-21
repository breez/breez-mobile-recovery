package core

import (
	"archive/zip"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// readZip returns the node files found in a backup zip, keyed by base name.
func readZip(zipPath string) (map[string][]byte, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(zipPath), err)
	}
	defer r.Close()
	return unzipNodeFiles(&r.Reader)
}

// zipIsEncrypted reports whether the node files in the zip are ciphertext.
func zipIsEncrypted(zipPath string) (bool, error) {
	files, err := readZip(zipPath)
	if err != nil {
		return false, err
	}
	if err := checkComplete(files); err != nil {
		return false, err
	}
	for name, content := range files {
		if looksLikeCiphertext(name, content) {
			return true, nil
		}
	}
	return false, nil
}

func nodeFileTargets(network string) map[string]string {
	return map[string]string{
		"wallet.db":  filepath.Join("data", "chain", "bitcoin", network),
		"channel.db": filepath.Join("data", "graph", network),
		"breez.db":   "",
	}
}

func checkComplete(files map[string][]byte) error {
	var missing []string
	for name := range nodeFileTargets("") {
		if _, ok := files[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the backup is missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// decryptGCM mirrors backup/crypto.go: 12-byte nonce prefix, AES-256-GCM.
func decryptGCM(content, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(content) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := content[:gcm.NonceSize()], content[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// looksLikeCiphertext is a cheap sanity check: the node files are bbolt/sqlite
// databases with recognisable headers.
func looksLikeCiphertext(name string, content []byte) bool {
	if len(content) < 16 {
		return true
	}
	switch name {
	case "breez.db":
		return string(content[:6]) != "SQLite" && !isBolt(content)
	default:
		return !isBolt(content)
	}
}

// isBolt checks the bbolt magic number at offset 16 of page 0 (0xED0CDAED LE).
func isBolt(content []byte) bool {
	if len(content) < 20 {
		return false
	}
	return content[16] == 0xED && content[17] == 0xDA && content[18] == 0x0C && content[19] == 0xED
}
