package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestMessengerVerifySignature(t *testing.T) {
	secret := "s3cr3t"
	gw := NewMetaMessenger("page-token", secret)
	body := []byte(`{"object":"page"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !gw.VerifySignature(body, good) {
		t.Error("valid signature rejected")
	}
	if gw.VerifySignature(body, "sha256=deadbeef") {
		t.Error("bad signature accepted")
	}
	if gw.VerifySignature(body, "") {
		t.Error("empty signature accepted")
	}
	if gw.VerifySignature(body, "md5=abc") {
		t.Error("non-sha256 prefix accepted")
	}
	if gw.VerifySignature([]byte(`{"object":"tampered"}`), good) {
		t.Error("signature valid for a different body")
	}
}

// With no app secret configured (dev/mock), verification passes through so local
// testing isn't blocked.
func TestMessengerVerifySignatureNoSecret(t *testing.T) {
	gw := NewMetaMessenger("page-token", "")
	if !gw.VerifySignature([]byte("anything"), "") {
		t.Error("no-secret gateway should pass verification through")
	}
}

// A blank PSID or missing page token must never attempt a send.
func TestMessengerSendGuards(t *testing.T) {
	gw := NewMetaMessenger("", "")
	if r, _ := gw.Send("psid", "hi"); r.OK {
		t.Error("send with no page token should be a no-op")
	}
	gw2 := NewMetaMessenger("tok", "")
	if r, _ := gw2.Send("", "hi"); r.OK {
		t.Error("send with empty psid should be a no-op")
	}
}
