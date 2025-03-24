package common

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// CreateTLSConfig creates a tls.Config based on the provided TLS settings.
func CreateTLSConfig(tlsCfg *TLSConfig) (*tls.Config, error) {
	config := &tls.Config{
		InsecureSkipVerify: tlsCfg.InsecureSkipVerify, // #nosec G402
	}

	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert/key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{cert}
	}

	if tlsCfg.CAFile != "" {
		caCert, err := os.ReadFile(tlsCfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		config.RootCAs = caCertPool
	}

	return config, nil
}
