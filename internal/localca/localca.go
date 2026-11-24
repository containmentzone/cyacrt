// Package localca issues the proxy's certificates from a local CA, for
// hosts that can't reach Let's Encrypt.
package localca

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Leaves live a month and are reissued a week before they expire. They
// are only kept in memory; a restart issues new ones.
const (
	leafLife  = 30 * 24 * time.Hour
	leafRenew = 7 * 24 * time.Hour
)

// CA signs a leaf for every host its policy allows, at the first
// handshake for it.
type CA struct {
	cert   *x509.Certificate
	signer crypto.Signer
	// HostPolicy refuses hosts that get no certificate, as autocert's does.
	HostPolicy func(context.Context, string) error

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// Load reads a CA from PEM files: a certificate and its private key
// (PKCS#1, PKCS#8 or SEC 1).
func Load(certFile, keyFile string) (*CA, error) {
	kp, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("local CA: %w", err)
	}
	if !kp.Leaf.IsCA {
		return nil, fmt.Errorf("local CA: %s is not a CA certificate", certFile)
	}
	signer, ok := kp.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("local CA: %s: unsupported key type", keyFile)
	}
	return &CA{cert: kp.Leaf, signer: signer}, nil
}

// LoadOrCreate loads dir/ca.crt and dir/ca.key, first generating them if
// neither exists. A generated CA is name-constrained to domain and its
// subdomains, so trusting it can't vouch for any other site.
func LoadOrCreate(dir, domain string) (*CA, error) {
	certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	_, errC := os.Stat(certFile)
	_, errK := os.Stat(keyFile)
	if errors.Is(errC, os.ErrNotExist) && errors.Is(errK, os.ErrNotExist) {
		if err := create(dir, certFile, keyFile, domain); err != nil {
			return nil, fmt.Errorf("local CA: %w", err)
		}
	}
	ca, err := Load(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if c := ca.cert.PermittedDNSDomains; len(c) > 0 && !strings.EqualFold(c[0], domain) {
		return nil, fmt.Errorf("local CA: %s only covers %s, not %s; delete %s to make a new one", certFile, c[0], domain, dir)
	}
	return ca, nil
}

func create(dir, certFile, keyFile, domain string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "cyac local CA for " + domain},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.AddDate(10, 0, 0),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The key first: a certificate without it would stop the next start.
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// PEM is the CA certificate, for clients to trust.
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Fingerprint is the SHA-256 of the CA certificate, to check a copy against.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// GetCertificate is a tls.Config.GetCertificate.
func (ca *CA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if host == "" {
		return nil, errors.New("local CA: no server name")
	}
	if err := ca.HostPolicy(hello.Context(), host); err != nil {
		return nil, err
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c := ca.leaves[host]; c != nil && time.Until(c.Leaf.NotAfter) > leafRenew {
		return c, nil
	}
	c, err := ca.issue(host)
	if err != nil {
		return nil, fmt.Errorf("local CA: %s: %w", host, err)
	}
	if ca.leaves == nil {
		ca.leaves = map[string]*tls.Certificate{}
	}
	ca.leaves[host] = c
	return c, nil
}

func (ca *CA) issue(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.signer)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
