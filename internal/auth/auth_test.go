package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestCredentialEncodingAndHash(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("repeated random credential")
	}
	a, err := Hash(first)
	if err != nil || len(a) != 32 {
		t.Fatal("invalid generated credential")
	}
	b, _ := Hash(first)
	if !bytes.Equal(a, b) || bytes.Contains(a, []byte(first)) {
		t.Fatal("invalid credential hash")
	}
	for _, token := range []string{"", "wr_", strings.Repeat("x", 46), first + " ", first + "\n", "wr_" + strings.Repeat("A", 42) + "B"} {
		if _, err := Hash(token); err == nil {
			t.Fatal("accepted malformed credential")
		}
	}
}
