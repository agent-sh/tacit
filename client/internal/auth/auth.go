// Package auth holds the client's Ed25519 writer key and mints the
// short-lived push tokens the server verifies (see server
// TacitServer.Auth.Token and SPEC.md "keys/").
//
//	message = "tacit-v1\n" + keyid + "\n" + ts + "\n" + nonce
//	token   = ts + "." + nonce + "." + base64url(ed25519_sign(message))
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Key is one writer identity: `<harness>@<host>` plus its key pair.
type Key struct {
	ID   string
	Priv ed25519.PrivateKey
}

// Dir is where keys live: $TACIT_CONFIG or ~/.config/tacit.
func Dir() (string, error) {
	if d := os.Getenv("TACIT_CONFIG"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "tacit"), nil
}

// Generate creates and stores a key for id. It refuses to overwrite.
func Generate(dir, id string) (*Key, error) {
	path := filepath.Join(dir, id+".key")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s exists; remove it to rotate", path)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	k := &Key{ID: id, Priv: priv}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".pub"), []byte(k.PublicOpenSSH()+"\n"), 0o644); err != nil {
		return nil, err
	}
	return k, nil
}

// Load reads the key for id from dir.
func Load(dir, id string) (*Key, error) {
	b, err := os.ReadFile(filepath.Join(dir, id+".key"))
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(trimNL(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("key file is not a base64 ed25519 seed")
	}
	return &Key{ID: id, Priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicOpenSSH renders `ssh-ed25519 <base64 blob> <id>`, the form the
// server stores under keys/<id>.pub.
func (k *Key) PublicOpenSSH() string {
	pub := k.Priv.Public().(ed25519.PublicKey)
	blob := make([]byte, 0, 4+11+4+32)
	blob = binary.BigEndian.AppendUint32(blob, 11)
	blob = append(blob, "ssh-ed25519"...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	blob = append(blob, pub...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + k.ID
}

// Token mints a push token valid around now.
func (k *Key) Token(now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	var nb [12]byte
	if _, err := rand.Read(nb[:]); err != nil {
		panic(err) // the system RNG failing is not recoverable here
	}
	nonce := base64.RawURLEncoding.EncodeToString(nb[:])
	sig := ed25519.Sign(k.Priv, []byte("tacit-v1\n"+k.ID+"\n"+ts+"\n"+nonce))
	return ts + "." + nonce + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
