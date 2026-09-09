package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tyler-smith/go-bip39"
	"github.com/zenon-network/go-zenon/wallet"
)

// KeySource is where the airdropping key comes from. Exactly one of the paths
// is used, and neither the phrase nor the password is written anywhere by this
// program.
type KeySource struct {
	// Mnemonic is a BIP-39 phrase.
	Mnemonic string
	// KeyFilePath is an encrypted go-zenon keystore; Password decrypts it.
	KeyFilePath string
	Password    string
	// Index selects the account under m/44'/73404'/index'.
	Index uint32
}

// LoadKey derives the signing key.
func LoadKey(src KeySource) (*wallet.KeyPair, error) {
	switch {
	case src.Mnemonic != "":
		return fromMnemonic(src.Mnemonic, src.Index)
	case src.KeyFilePath != "":
		kf, err := wallet.ReadKeyFile(src.KeyFilePath)
		if err != nil {
			return nil, fmt.Errorf("read keystore: %w", err)
		}
		ks, err := kf.Decrypt(src.Password)
		if err != nil {
			return nil, fmt.Errorf("decrypt keystore: %w", err)
		}
		_, kp, err := ks.DeriveForIndexPath(src.Index)
		if err != nil {
			return nil, err
		}
		return kp, nil
	default:
		return nil, fmt.Errorf("no key source: pass a keystore or a mnemonic")
	}
}

func fromMnemonic(mnemonic string, index uint32) (*wallet.KeyPair, error) {
	mnemonic = strings.Join(strings.Fields(mnemonic), " ")
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("invalid BIP-39 mnemonic")
	}
	// The empty passphrase matches how go-zenon derives its own seed
	// (keyStoreFromEntropy uses bip39.NewSeed(mnemonic, "")), so an address
	// derived here equals the one the node's keystore would produce.
	seed := bip39.NewSeed(mnemonic, "")
	kp, err := wallet.DeriveForPath(fmt.Sprintf("m/44'/73404'/%d'", index), seed)
	if err != nil {
		return nil, err
	}
	return kp, nil
}

// ReadMnemonicFile loads a phrase from disk, ignoring blank lines and
// #-comments so a phrase file can carry a note about which network it is for.
func ReadMnemonicFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var words []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " "), nil
}
