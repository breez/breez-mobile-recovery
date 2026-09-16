package core

import (
	"archive/zip"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"os"
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

	files := map[string][]byte{}
	for _, f := range r.File {
		name := filepath.Base(f.Name)
		if _, ok := nodeFileTargets("")[name]; !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		files[name] = content
	}
	return files, nil
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

// restoreFromZip places the three node files from a backup zip (as uploaded
// by the mobile app) into the lnd directory layout under the work dir. It
// mirrors backup.Manager.restoreNodeData, which is not reachable without a
// provider.
//
// key == nil means the backup is not encrypted.
func (c *Core) restoreFromZip(zipPath string, key []byte) error {
	files, err := readZip(zipPath)
	if err != nil {
		return err
	}
	return c.restoreFiles(files, key)
}

// restoreFromPaths restores from loose files (legacy iCloud records store
// wallet.db, channel.db and breez.db as three separate assets).
func (c *Core) restoreFromPaths(paths []string, key []byte) error {
	files := map[string][]byte{}
	for _, p := range paths {
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.Base(p)] = content
	}
	return c.restoreFiles(files, key)
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

// restoreFiles decrypts (when key != nil) and places the node files into the
// lnd directory layout under the work dir.
func (c *Core) restoreFiles(files map[string][]byte, key []byte) error {
	if err := checkComplete(files); err != nil {
		return err
	}
	targets := nodeFileTargets(c.cfg.Network)
	for name, content := range files {
		rel := targets[name]
		var err error
		if key != nil {
			content, err = decryptGCM(content, key)
			if err != nil {
				return fmt.Errorf("decrypt %s: %w (wrong backup phrase?)", name, err)
			}
		} else if looksLikeCiphertext(name, content) {
			return fmt.Errorf("%s does not look like a database; the backup is encrypted and needs the backup phrase", name)
		}
		destDir := filepath.Join(c.cfg.WorkDir, rel)
		if err := os.MkdirAll(destDir, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destDir, name), content, 0600); err != nil {
			return err
		}
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
