package main

import (
	"encoding/base64"
	"testing"
)

func TestCosyAESEncryptMatchesNode(t *testing.T) {
	key := "0123456789abcdef"
	plain := `{"name":"测试","uid":"u1","aid":"a1"}`
	want := "zvSibBmMDYx0MCU+Tw06jmgARMIYFgZ/ap4/lI0k2CcQ7RMFt4AmZXxCcOZQf8BO"
	got, err := cosyAESEncrypt(plain, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if encoded := base64.StdEncoding.EncodeToString(got); encoded != want {
		t.Fatalf("ciphertext = %q, want %q", encoded, want)
	}
}

func TestMD5HexMatchesNode(t *testing.T) {
	got := md5Hex("abc\nkey\ndate\nbody\n/path")
	if want := "20db26f5ea1ddba0b4df73df4f2aad47"; got != want {
		t.Fatalf("md5Hex = %q, want %q", got, want)
	}
}
