// Package certs issues the certificates that authenticate agents.
//
// Enrolment is operator-driven: an administrator generates a CA once, then
// issues one client certificate per cPanel server and registers its
// fingerprint. There is no automatic enrolment endpoint, because anything
// that hands out credentials to whoever asks is a way into every backup.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Pair is a certificate and its private key, PEM encoded.
type Pair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// WriteFiles saves the pair, giving the private key owner-only permissions.
func (p Pair) WriteFiles(certPath, keyPath string) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return fmt.Errorf("certs: create directory: %w", err)
	}
	if err := os.WriteFile(certPath, p.CertPEM, 0o644); err != nil {
		return fmt.Errorf("certs: write certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, p.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("certs: write key: %w", err)
	}
	return nil
}

// Authority is a signing CA.
type Authority struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	Pair Pair
}

// NewAuthority creates a self-signed CA.
func NewAuthority(commonName string, validFor time.Duration) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generate ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"cprest"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certs: sign ca: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certs: parse ca: %w", err)
	}
	pair, err := encodePair(der, key)
	if err != nil {
		return nil, err
	}
	return &Authority{Cert: cert, Key: key, Pair: pair}, nil
}

// LoadAuthority reads a CA from disk.
func LoadAuthority(certPath, keyPath string) (*Authority, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("certs: read ca certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("certs: read ca key: %w", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("certs: ca files are not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certs: parse ca certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certs: parse ca key: %w", err)
	}
	return &Authority{
		Cert: cert, Key: key,
		Pair: Pair{CertPEM: certPEM, KeyPEM: keyPEM},
	}, nil
}

// IssueServer signs a certificate for the controller's own listener.
func (a *Authority) IssueServer(commonName string, hosts []string, validFor time.Duration) (Pair, error) {
	return a.issue(commonName, hosts,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, validFor)
}

// IssueClient signs a certificate for one agent. The common name carries
// the hostname purely for human legibility; authorisation comes from the
// fingerprint registered against the server record, not from the name.
func (a *Authority) IssueClient(commonName string, validFor time.Duration) (Pair, error) {
	return a.issue(commonName, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, validFor)
}

func (a *Authority) issue(commonName string, hosts []string,
	usage []x509.ExtKeyUsage, validFor time.Duration) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, fmt.Errorf("certs: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Pair{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"cprest"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.Cert, &key.PublicKey, a.Key)
	if err != nil {
		return Pair{}, fmt.Errorf("certs: sign certificate: %w", err)
	}
	return encodePair(der, key)
}

// Fingerprint is the SHA-256 of a certificate's DER encoding, lowercase
// hex. This is the identity the controller pins per server.
func Fingerprint(cert *x509.Certificate) string {
	digest := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(digest[:])
}

// FingerprintPEM computes the fingerprint of the first certificate in a PEM
// blob, for the CLI that registers a server.
func FingerprintPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("certs: not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("certs: parse certificate: %w", err)
	}
	return Fingerprint(cert), nil
}

func encodePair(der []byte, key *ecdsa.PrivateKey) (Pair, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Pair{}, fmt.Errorf("certs: marshal key: %w", err)
	}
	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("certs: serial: %w", err)
	}
	return serial, nil
}
