package secret

import (
	"bytes"
	"testing"
)

func TestBoxEncryptsAndDecrypts(t *testing.T) {
	t.Parallel()
	box, err := NewBox(bytes.Repeat([]byte{0x2a}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("synthetic-endpoint-secret")
	first, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first, plaintext) || bytes.Equal(first, second) {
		t.Fatal("ciphertext exposed plaintext or reused a nonce")
	}
	decrypted, err := box.Decrypt(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted %q, want %q", decrypted, plaintext)
	}
}
