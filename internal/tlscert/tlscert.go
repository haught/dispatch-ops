// Package tlscert makes and loads the API server's TLS certificate: a
// self-signed ECDSA P-384 certificate when none is provided, or a key pair
// from files (such as a mounted cert-manager Secret) that is reloaded when
// the files change.
package tlscert

import (
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
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// SelfSigned returns a new self-signed server certificate and its ECDSA
// P-384 private key, both PEM-encoded, valid for hosts (DNS names or IP
// addresses) for the given duration. The first host is the subject's
// common name.
func SelfSigned(hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	tmpl, err := serverTemplate(hosts, validity)
	if err != nil {
		return nil, nil, err
	}
	tmpl.Subject.Organization = []string{"Dispatch Ops (self-signed)"}
	return issue(tmpl, nil, nil)
}

// CA returns a new self-signed CA certificate and its ECDSA P-384 private
// key, both PEM-encoded, valid for the given duration, for Signed to issue
// server certificates with.
func CA(commonName string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Dispatch Ops"}},
		NotBefore:             now.Add(-time.Hour), // tolerate clock skew
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	return issue(tmpl, nil, nil)
}

// Signed returns a new server certificate for hosts, valid for the given
// duration, and its ECDSA P-384 private key, both PEM-encoded, issued by the
// CA whose PEM certificate and key (from CA) are given.
func Signed(hosts []string, validity time.Duration, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	ca, err := ParseCertificate(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: CA certificate: %w", err)
	}
	block, _ := pem.Decode(caKeyPEM)
	if block == nil {
		return nil, nil, errors.New("tlscert: CA key: no PEM block")
	}
	caKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: CA key: %w", err)
	}
	tmpl, err := serverTemplate(hosts, validity)
	if err != nil {
		return nil, nil, err
	}
	return issue(tmpl, ca, caKey)
}

// ParseCertificate parses the first certificate in PEM data.
func ParseCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// serverTemplate is a server certificate for hosts, valid from now.
func serverTemplate(hosts []string, validity time.Duration) (*x509.Certificate, error) {
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		return nil, errors.New("tlscert: at least one host is required")
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		NotBefore:             now.Add(-time.Hour), // tolerate clock skew
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return tmpl, nil
}

// issue makes a new ECDSA P-384 key and a certificate for it from tmpl,
// signed by parent's key, or self-signed if parent is nil.
func issue(tmpl, parent *x509.Certificate, parentKey any) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func uniqueHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// Fingerprint is the SHA-256 fingerprint of a certificate's DER bytes, as
// colon-separated hex, the way browsers and openssl show it.
func Fingerprint(cert *tls.Certificate) string {
	if cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// Reloader serves a key pair from files, reloading it when either file
// changes. Kubernetes updates a mounted Secret's files in place, so a
// renewed certificate (from cert-manager, say) is served to new
// connections without a restart.
type Reloader struct {
	certFile, keyFile string
	log               *slog.Logger
	checkEvery        time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

// NewReloader loads the key pair, failing if it can't.
func NewReloader(certFile, keyFile string, log *slog.Logger) (*Reloader, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, log: log, checkEvery: 30 * time.Second}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// Certificate returns the current key pair.
func (r *Reloader) Certificate() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert
}

// GetCertificate is a tls.Config GetCertificate function.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastCheck) >= r.checkEvery {
		r.lastCheck = time.Now()
		if r.changed() {
			if err := r.loadLocked(); err != nil {
				r.log.Warn("reloading the TLS certificate failed; still serving the previous one",
					"cert_file", r.certFile, "err", err)
			} else {
				r.log.Info("reloaded the TLS certificate", "cert_file", r.certFile, "sha256", Fingerprint(r.cert))
			}
		}
	}
	return r.cert, nil
}

func (r *Reloader) changed() bool {
	cm, err1 := modTime(r.certFile)
	km, err2 := modTime(r.keyFile)
	if err1 != nil || err2 != nil {
		return false
	}
	return !cm.Equal(r.certMod) || !km.Equal(r.keyMod)
}

func (r *Reloader) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCheck = time.Now()
	return r.loadLocked()
}

func (r *Reloader) loadLocked() error {
	cm, err := modTime(r.certFile)
	if err != nil {
		return err
	}
	km, err := modTime(r.keyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	r.cert, r.certMod, r.keyMod = &cert, cm, km
	return nil
}

// modTime follows symlinks, as Kubernetes' Secret volumes use them.
func modTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// ReadTrustBundle reads the certificates a client should trust to reach a
// server whose kubernetes.io/tls Secret is mounted at dir: its ca.crt, or
// its tls.crt when there is no (or an empty) ca.crt, as with certificates
// from a public CA.
func ReadTrustBundle(dir string) ([]byte, error) {
	for _, name := range []string{"ca.crt", "tls.crt"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(b))) == 0) {
			continue
		}
		return b, err
	}
	return nil, fmt.Errorf("neither ca.crt nor tls.crt in %s", dir)
}

// TrustPool returns the system's trusted roots plus the PEM certificates in
// extra.
func TrustPool(extra []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(extra) > 0 && !pool.AppendCertsFromPEM(extra) {
		return nil, errors.New("tlscert: no PEM certificates found")
	}
	return pool, nil
}
