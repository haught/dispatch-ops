package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := SelfSigned([]string{"api.ns.svc", "localhost", "127.0.0.1", "localhost", " "}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := parse(t, certPEM)
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P384() {
		t.Fatalf("public key is %T, want ECDSA P-384", c.PublicKey)
	}
	if !slices.Equal(c.DNSNames, []string{"api.ns.svc", "localhost"}) {
		t.Errorf("DNS names = %v", c.DNSNames)
	}
	if len(c.IPAddresses) != 1 || c.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("IP addresses = %v", c.IPAddresses)
	}
	if c.Subject.CommonName != "api.ns.svc" {
		t.Errorf("common name = %q", c.Subject.CommonName)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("key pair: %v", err)
	}

	// Trusting the certificate itself is enough to verify it.
	pool, err := TrustPool(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.ns.svc", "127.0.0.1"} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example"}); err == nil {
		t.Error("verified for a host not in the certificate")
	}

	if _, _, err := SelfSigned(nil, time.Hour); err == nil {
		t.Error("no hosts: want an error")
	}
}

func writePair(t *testing.T, dir, host string) {
	t.Helper()
	certPEM, keyPEM, err := SelfSigned([]string{host}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func leafHost(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x.Subject.CommonName
}

func TestReloader(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "one.example")
	r, err := NewReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.checkEvery = 0
	got, _ := r.GetCertificate(nil)
	if h := leafHost(t, got); h != "one.example" {
		t.Fatalf("serving %s", h)
	}

	writePair(t, dir, "two.example")
	// Make sure the modification time differs on coarse-grained filesystems.
	later := time.Now().Add(time.Minute)
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Chtimes(filepath.Join(dir, f), later, later); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = r.GetCertificate(nil)
	if h := leafHost(t, got); h != "two.example" {
		t.Fatalf("after renewal, serving %s", h)
	}

	// A broken file keeps the previous certificate.
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	evenLater := later.Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "tls.crt"), evenLater, evenLater); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetCertificate(nil)
	if h := leafHost(t, got); h != "two.example" {
		t.Fatalf("after a bad update, serving %s", h)
	}

	if _, err := NewReloader(filepath.Join(dir, "missing.crt"), filepath.Join(dir, "tls.key"), nil); err == nil {
		t.Error("missing file: want an error")
	}
}

func TestReadTrustBundle(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadTrustBundle(dir); err == nil {
		t.Error("empty dir: want an error")
	}
	os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("leaf"), 0o600)
	os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(""), 0o600)
	if b, err := ReadTrustBundle(dir); err != nil || string(b) != "leaf" {
		t.Errorf("without a CA: %q, %v", b, err)
	}
	os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("ca"), 0o600)
	if b, err := ReadTrustBundle(dir); err != nil || string(b) != "ca" {
		t.Errorf("with a CA: %q, %v", b, err)
	}
}

func TestFingerprint(t *testing.T) {
	certPEM, keyPEM, _ := SelfSigned([]string{"x"}, time.Hour)
	c, _ := tls.X509KeyPair(certPEM, keyPEM)
	if fp := Fingerprint(&c); len(fp) != 32*3-1 {
		t.Errorf("fingerprint %q", fp)
	}
}

func TestSignedByCA(t *testing.T) {
	caPEM, caKey, err := CA("test CA", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseCertificate(caPEM)
	if err != nil || !ca.IsCA {
		t.Fatalf("CA = %+v, %v", ca, err)
	}
	certPEM, keyPEM, err := Signed([]string{"db.ns.svc", "127.0.0.1"}, time.Hour, caPEM, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	leaf, err := ParseCertificate(certPEM)
	if err != nil || leaf.IsCA {
		t.Fatalf("leaf = %+v, %v", leaf, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "db.ns.svc", Roots: roots}); err != nil {
		t.Errorf("leaf doesn't verify against its CA: %v", err)
	}
	// Another CA doesn't vouch for it.
	otherPEM, _, _ := CA("other", time.Hour)
	other, _ := ParseCertificate(otherPEM)
	roots = x509.NewCertPool()
	roots.AddCert(other)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "db.ns.svc", Roots: roots}); err == nil {
		t.Error("leaf verifies against an unrelated CA")
	}
	if _, _, err := Signed([]string{"x"}, time.Hour, caPEM, []byte("junk")); err == nil {
		t.Error("signed with a bad CA key")
	}
}
