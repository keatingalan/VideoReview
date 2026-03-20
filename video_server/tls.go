package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"os"
	"strings"
	"time"
)

// loadOrCreateCert loads cert+key from disk, generating a new self-signed
// certificate if either file is missing. The cert has no IP SANs so it remains
// valid if the server's IP changes between runs. Clients see a browser warning
// on first connection and need to click through once per device.
func loadOrCreateCert() tls.Certificate {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		log.Printf("Loaded TLS certificate from %s", certFile)
		return cert
	}

	log.Printf("No certificate found (%v) — generating a new self-signed certificate", err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate private key: %v", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "WAG-Video-Review"},
		Issuer:       pkix.Name{CommonName: "WAG-Video-Review"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("Failed to create certificate: %v", err)
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		log.Fatalf("Failed to write cert file: %v", err)
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("Failed to marshal private key: %v", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		log.Fatalf("Failed to write key file: %v", err)
	}
	os.Chmod(keyFile, 0600)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	keyOut.Close()

	log.Printf("Generated new self-signed certificate → %s", certFile)

	cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load generated certificate: %v", err)
	}
	return cert
}

// tlsErrorFilter silently drops "TLS handshake error: unknown certificate"
// log lines produced when a browser rejects the self-signed cert before the
// user has clicked through the warning. Everything else passes through.
type tlsErrorFilter struct{ w io.Writer }

func (f tlsErrorFilter) Write(p []byte) (n int, err error) {
	if strings.Contains(string(p), "TLS handshake error") &&
		strings.Contains(string(p), "unknown certificate") {
		return len(p), nil
	}
	return f.w.Write(p)
}
