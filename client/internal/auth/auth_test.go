package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestGenerateLoadToken(t *testing.T) {
	dir := t.TempDir()
	k, err := Generate(dir, "claude@host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir, "claude@host-a"); err == nil {
		t.Fatal("overwrite allowed")
	}
	l, err := Load(dir, "claude@host-a")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Priv.Equal(k.Priv) {
		t.Fatal("loaded key differs")
	}
	pub := k.PublicOpenSSH()
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " claude@host-a") {
		t.Fatalf("pub %q", pub)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.Fields(pub)[1])
	if err != nil || len(blob) != 4+11+4+32 || string(blob[4:15]) != "ssh-ed25519" {
		t.Fatalf("blob %x", blob)
	}
	want := k.Priv.Public().(ed25519.PublicKey)
	if string(blob[19:]) != string(want) {
		t.Fatal("blob key mismatch")
	}

	now := time.Unix(1_000, 0)
	tok := k.Token(now)
	parts := strings.SplitN(tok, ".", 3)
	if len(parts) != 3 || parts[0] != "1000" {
		t.Fatalf("token %q", tok)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("tacit-v1\nclaude@host-a\n1000\n" + parts[1])
	if !ed25519.Verify(want, msg, sig) {
		t.Fatal("signature does not verify against the server's message form")
	}
	if k.Token(now) == tok {
		t.Fatal("nonce reused")
	}
}
