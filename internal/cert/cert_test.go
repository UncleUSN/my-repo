package cert

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"
)

func TestGenerateTrustedByItself(t *testing.T) {
	dir := t.TempDir()
	res, err := Generate(Options{Dir: dir, Hosts: []string{"ferry.local"}, IPs: []string{"10.0.0.8"}, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(res.CertFile, res.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "ferry" {
		t.Fatalf("cn %s", leaf.Subject.CommonName)
	}
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "10.0.0.8" || ip.String() == "127.0.0.1" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing IP SAN")
	}
	pemBytes, err := os.ReadFile(res.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("pool")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatal(err)
	}
}
