package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/UncleUSN/my-repo/internal/config"
)

func transportFor(p config.Peer) (*http.Transport, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.TLSSkipVerify {
		// Осознанный режим лаборатории: сертификат не проверяется.
		// Токен при этом всё равно уходит только внутрь TLS-сессии,
		// просто клиент не убеждается, кто на другом конце.
		tlsCfg.InsecureSkipVerify = true
	} else {
		if p.CAFile == "" {
			return nil, fmt.Errorf("peer %s: set ca_file or tls_skip_verify", p.ID)
		}
		b, err := os.ReadFile(p.CAFile)
		if err != nil {
			return nil, fmt.Errorf("peer %s: ca_file: %w", p.ID, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("peer %s: ca_file %s has no PEM certificates", p.ID, p.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Transport{
		TLSClientConfig:       tlsCfg,
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}, nil
}
