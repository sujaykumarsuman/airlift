package tlsca

import (
	"bytes"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFreshCAOnEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "airlift")
	ca, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created || ca.Cert == nil || ca.Key == nil || len(ca.CertPEM) == 0 {
		t.Fatalf("created=%v ca=%+v", created, ca)
	}
	if !ca.Cert.IsCA || ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("certificate is not a signing CA")
	}
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %04o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
}

func TestExistingCAReused(t *testing.T) {
	dir := t.TempDir()
	first, created, err := LoadOrCreate(dir)
	if err != nil || !created {
		t.Fatalf("first: %v created=%v", err, created)
	}
	second, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second call created a new CA")
	}
	if !bytes.Equal(first.CertPEM, second.CertPEM) || !first.Key.Equal(second.Key) {
		t.Fatal("reloaded CA differs")
	}
}

func TestLeafVerifiesForSANsOnly(t *testing.T) {
	ca, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf([]net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("10.1.2.3")}, nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Certificate) != 2 {
		t.Fatalf("chain has %d certificates, want leaf + CA", len(leaf.Certificate))
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("chain does not verify: %v", err)
	}
	for _, ok := range []string{"192.168.1.5", "10.1.2.3", "127.0.0.1", "localhost"} {
		if err := leaf.Leaf.VerifyHostname(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"192.168.1.6", "10.0.0.1", "example.com"} {
		if err := leaf.Leaf.VerifyHostname(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	other, _, _ := LoadOrCreate(t.TempDir())
	otherRoots := x509.NewCertPool()
	otherRoots.AddCert(other.Cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: otherRoots}); err == nil {
		t.Error("leaf verified against a different CA")
	}
}

func TestInsecureKeyRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dir); !errors.Is(err, ErrInsecureKey) {
		t.Fatalf("got %v, want ErrInsecureKey", err)
	}
}

func TestLoadPair(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreate(dir)
	if _, err := LoadPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPair(filepath.Join(dir, CertFile), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing key accepted")
	}
	_ = ca
}
