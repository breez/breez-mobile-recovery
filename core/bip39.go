package core

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"math/big"
	"strings"
)

// The Breez mobile app encrypts the backup with a key derived from the
// backup phrase (BIP39 mnemonic). This mirrors BreezLibBackupKey in
// lib/bloc/backup/backup_model.dart:
//
//	entropy := bip39.mnemonicToEntropy(phrase)      // 32 bytes for 24 words, 16 for 12
//	key     := entropy if len(entropy) == 32 else sha256(entropy)
//	type    := "Mnemonics" for 24 words, "Mnemonics12" for 12 words
//
// The mnemonic is NOT the lnd seed. lnd runs with noseedbackup=1 and the
// seed lives inside the backed up wallet.db.

//go:embed bip39_english.txt
var bip39EnglishRaw string

var bip39Index = func() map[string]int {
	m := make(map[string]int, 2048)
	for i, w := range strings.Split(strings.TrimSpace(bip39EnglishRaw), "\n") {
		m[strings.TrimSpace(w)] = i
	}
	return m
}()

// mnemonicToEntropy validates the BIP39 checksum and returns the entropy bytes.
func mnemonicToEntropy(mnemonic string) ([]byte, error) {
	words := strings.Fields(strings.ToLower(mnemonic))
	n := len(words)
	if n != 12 && n != 24 {
		return nil, fmt.Errorf("expected 12 or 24 words, got %d", n)
	}
	if len(bip39Index) != 2048 {
		return nil, fmt.Errorf("embedded wordlist has %d words, expected 2048", len(bip39Index))
	}

	bits := new(big.Int)
	for _, w := range words {
		idx, ok := bip39Index[w]
		if !ok {
			return nil, fmt.Errorf("%q is not a BIP39 word", w)
		}
		bits.Lsh(bits, 11)
		bits.Or(bits, big.NewInt(int64(idx)))
	}

	checksumBits := n / 3 // 4 for 12 words, 8 for 24 words
	entropyBytes := (n*11 - checksumBits) / 8

	checksum := new(big.Int).And(bits, big.NewInt(int64(1<<checksumBits-1)))
	entropyInt := bits.Rsh(bits, uint(checksumBits))
	entropy := make([]byte, entropyBytes)
	entropyInt.FillBytes(entropy)

	h := sha256.Sum256(entropy)
	want := int64(h[0] >> (8 - checksumBits))
	if checksum.Int64() != want {
		return nil, fmt.Errorf("mnemonic checksum mismatch, please check the words")
	}
	return entropy, nil
}

// backupKeyFromMnemonic returns the AES key and the encryption type label the
// mobile app stores next to the snapshot.
func backupKeyFromMnemonic(mnemonic string) (key []byte, encType string, err error) {
	entropy, err := mnemonicToEntropy(mnemonic)
	if err != nil {
		return nil, "", err
	}
	if len(entropy) == 32 {
		return entropy, "Mnemonics", nil
	}
	sum := sha256.Sum256(entropy)
	return sum[:], "Mnemonics12", nil
}
