package localca

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	ca, err := LoadOrCreate(dir, "boxes.example.com")
	if err != nil {
		t.Fatal(err)
	}
	ca.HostPolicy = func(_ context.Context, host string) error {
		if host == "evil.com" {
			return errors.New("unknown host")
		}
		return nil
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM())

	for _, host := range []string{"app.web.boxes.example.com", "boxes.example.com"} {
		c, err := ca.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
		if len(c.Certificate) != 2 {
			t.Errorf("%s: chain has %d certificates, want leaf and CA", host, len(c.Certificate))
		}
		if again, _ := ca.GetCertificate(&tls.ClientHelloInfo{ServerName: host}); again != c {
			t.Errorf("%s: leaf not cached", host)
		}
	}
	if _, err := ca.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.com"}); err == nil {
		t.Error("issued a leaf the policy refuses")
	}
	// The name constraint holds even for a leaf the CA was made to sign.
	leaf, err := ca.issue("example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "example.org", Roots: roots}); err == nil {
		t.Error("CA vouches for a name outside its domain")
	}

	// A restart loads the same CA; another domain refuses it.
	same, err := LoadOrCreate(dir, "boxes.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if same.Fingerprint() != ca.Fingerprint() {
		t.Error("CA was made again")
	}
	if _, err := LoadOrCreate(dir, "other.example.com"); err == nil {
		t.Error("loaded a CA constrained to another domain")
	}
	if _, err := Load(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")); err != nil {
		t.Error(err)
	}

	// A key left without its certificate (a failed first start) is replaced.
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadOrCreate(dir, "boxes.example.com"); err != nil || again.Fingerprint() == ca.Fingerprint() {
		t.Errorf("recovering from a lone key: %v", err)
	}
}
