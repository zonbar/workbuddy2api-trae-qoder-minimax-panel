package panel

import (
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
)

func TestQRDataURL(t *testing.T) {
	got, err := qrDataURL("https://example.com/auth?state=abc123")
	if err != nil {
		t.Fatalf("qrDataURL: %v", err)
	}
	if !strings.HasPrefix(got, qrDataURIPrefix) {
		t.Fatalf("unexpected prefix %q", got[:min(24, len(got))])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, qrDataURIPrefix))
	if err != nil {
		t.Fatalf("decode data uri: %v", err)
	}
	if _, err := png.Decode(strings.NewReader(string(raw))); err != nil {
		t.Fatalf("decode png: %v", err)
	}
}

func TestQRDataURLRejectsBadInput(t *testing.T) {
	if _, err := qrDataURL(""); err == nil {
		t.Fatal("empty url should fail")
	}
	if _, err := qrDataURL(strings.Repeat("x", qrMaxURLLen+1)); err == nil {
		t.Fatal("oversized url should fail")
	}
}
