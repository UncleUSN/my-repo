// Package cert выпускает самоподписанный сертификат для агента.
// Для лаборатории (Windows + WSL) этого достаточно. В бою лучше свой CA
// и ca_file у каждого peer вместо tls_skip_verify.
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Options задаёт, куда положить сертификат и какие имена в него вписать.
type Options struct {
	Dir        string
	Hosts      []string
	IPs        []string
	Days       int
	CommonName string
}

// Result — пути созданных файлов.
type Result struct {
	CertFile string
	KeyFile  string
}

// Generate создаёт ECDSA P-256 сертификат и ключ (cert.pem / key.pem).
// localhost и 127.0.0.1 добавляются всегда: CLI ходит на локальный агент именно так.
// Сертификат помечен как CA, чтобы его же можно было указать в ca_file.
func Generate(opts Options) (Result, error) {
	if opts.Dir == "" {
		return Result{}, fmt.Errorf("certificate directory is empty")
	}
	if opts.Days <= 0 {
		opts.Days = 825
	}
	if opts.CommonName == "" {
		opts.CommonName = "ferry"
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return Result{}, err
	}

	dns := []string{"localhost"}
	seenDNS := map[string]bool{"localhost": true}
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	seenIP := map[string]bool{"127.0.0.1": true}
	for _, h := range opts.Hosts {
		if h == "" || seenDNS[h] {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			if !seenIP[ip.String()] {
				ips = append(ips, ip)
				seenIP[ip.String()] = true
			}
			continue
		}
		dns = append(dns, h)
		seenDNS[h] = true
	}
	for _, s := range opts.IPs {
		ip := net.ParseIP(s)
		if ip == nil {
			return Result{}, fmt.Errorf("invalid IP %q", s)
		}
		if seenIP[ip.String()] {
			continue
		}
		ips = append(ips, ip)
		seenIP[ip.String()] = true
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Result{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Result{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: opts.CommonName, Organization: []string{"Ferry"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Duration(opts.Days) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Result{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Result{}, err
	}
	certPath := filepath.Join(opts.Dir, "cert.pem")
	keyPath := filepath.Join(opts.Dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return Result{}, err
	}
	return Result{CertFile: certPath, KeyFile: keyPath}, nil
}
