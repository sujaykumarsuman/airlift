package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// CertFile and KeyFile are the CA's file names inside the config dir.
	CertFile = "ca.crt"
	KeyFile  = "ca.key"

	caValidity = 10 * 365 * 24 * time.Hour
)

// ErrInsecureKey is returned when the persisted CA key is readable by others.
var ErrInsecureKey = errors.New("ca key file is readable by others")

// CA is the local authority: its certificate, private key and PEM encoding.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

// LoadOrCreate returns the CA persisted in dir, creating it on first use.
// The key is written with mode 0600 and refused on later runs if that has
// changed. created reports whether a new CA was generated.
func LoadOrCreate(dir string) (ca *CA, created bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	certPath := filepath.Join(dir, CertFile)
	keyPath := filepath.Join(dir, KeyFile)
	_, certErr := os.Stat(certPath)
	keyInfo, keyErr := os.Stat(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		if runtime.GOOS != "windows" && keyInfo.Mode().Perm()&0o077 != 0 {
			return nil, false, fmt.Errorf("%w: %s is %04o, want 0600", ErrInsecureKey, keyPath, keyInfo.Mode().Perm())
		}
		ca, err := load(certPath, keyPath)
		return ca, false, err
	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		ca, err := create(certPath, keyPath)
		return ca, true, err
	}
	return nil, false, fmt.Errorf("inconsistent CA state in %s: %v / %v", dir, certErr, keyErr)
}

func load(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: not a PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, _ = pem.Decode(keyPEM)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not a PEM EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

func create(certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "airlift local CA", Organization: []string{"airlift"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

// writeFile creates the file exclusively with the given mode, then enforces
// the mode in case the umask widened or narrowed it.
func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// Leaf issues a server certificate signed by the CA for the given IPs and
// DNS names; localhost and 127.0.0.1 are always included. The returned
// certificate carries the CA in its chain.
func (ca *CA) Leaf(ips []net.IP, dnsNames []string, validity time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	sans := []net.IP{net.IPv4(127, 0, 0, 1)}
	for _, ip := range ips {
		if ip != nil && !ip.Equal(sans[0]) {
			sans = append(sans, ip)
		}
	}
	names := []string{"localhost"}
	for _, n := range dnsNames {
		if n != "" && n != "localhost" {
			names = append(names, n)
		}
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "airlift tower", Organization: []string{"airlift"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  sans,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// LoadPair is the --cert/--key bypass: a certificate and key from disk, for
// example from mkcert.
func LoadPair(certFile, keyFile string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(certFile, keyFile)
}
