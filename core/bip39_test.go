package core

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestMnemonicToEntropy(t *testing.T) {
	// Vectors from the BIP39 reference test suite (trezor/python-mnemonic).
	cases := []struct {
		mnemonic string
		entropy  string
	}{
		{"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about", "00000000000000000000000000000000"},
		{"legal winner thank year wave sausage worth useful legal winner thank yellow", "7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f"},
		{"zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong", "ffffffffffffffffffffffffffffffff"},
		{"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art", "0000000000000000000000000000000000000000000000000000000000000000"},
		{"zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo vote", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
	}
	for _, c := range cases {
		got, err := mnemonicToEntropy(c.mnemonic)
		if err != nil {
			t.Fatalf("%q: %v", c.mnemonic, err)
		}
		if hex.EncodeToString(got) != c.entropy {
			t.Fatalf("%q: got %x want %s", c.mnemonic, got, c.entropy)
		}
	}
	// Upper case and extra whitespace are tolerated like the app's input field.
	if _, err := mnemonicToEntropy("  ZOO zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo   wrong "); err != nil {
		t.Fatal(err)
	}
}

func TestMnemonicRejects(t *testing.T) {
	bad := []string{
		"zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo",                                                 // bad checksum
		"abandon abandon abandon abandon abandon abandon",                                                 // wrong length
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon bitcoin", // not a word
	}
	for _, m := range bad {
		if _, err := mnemonicToEntropy(m); err == nil {
			t.Fatalf("expected error for %q", m)
		}
	}
}

// Mirrors BreezLibBackupKey.key in the Flutter app: 24 words use the raw
// 32-byte entropy, 12 words use sha256 of the 16-byte entropy.
func TestBackupKeyFromMnemonic(t *testing.T) {
	key, typ, err := backupKeyFromMnemonic(strings.Repeat("abandon ", 23) + "art")
	if err != nil {
		t.Fatal(err)
	}
	if typ != "Mnemonics" || !bytes.Equal(key, make([]byte, 32)) {
		t.Fatalf("24 words: type %s key %x", typ, key)
	}

	key, typ, err = backupKeyFromMnemonic(strings.Repeat("abandon ", 11) + "about")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(make([]byte, 16))
	if typ != "Mnemonics12" || !bytes.Equal(key, want[:]) {
		t.Fatalf("12 words: type %s key %x", typ, key)
	}
}

// Encrypt the way backup/crypto.go does and check we can open it.
func TestDecryptGCM(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := []byte("hello breez")
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{1}, 12)
	ct := gcm.Seal(nonce, nonce, plain, nil)

	got, err := decryptGCM(ct, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q", got)
	}
	if _, err := decryptGCM(ct, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("wrong key should fail")
	}
}
