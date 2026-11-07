package pg

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"testing"
)

func TestPinnedSQLPeerRejectsRedirectAndPreservesTLSVerification(t *testing.T) {
	leaf := &x509.Certificate{Raw: []byte("source-leaf")}
	h := sha256.Sum256(leaf.Raw)
	expected := hex.EncodeToString(h[:])
	config, err := pinPeer(&tls.Config{MinVersion: tls.VersionTLS12}, expected)
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS12 {
		t.Fatal("existing TLS constraints discarded")
	}
	if err = config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatal(err)
	}
	if err = config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("target-leaf")}}}); err == nil {
		t.Fatal("source DSN redirect to target accepted")
	}
	if err = config.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("absent peer accepted")
	}
	original := errors.New("original verification rejected")
	config, err = pinPeer(&tls.Config{VerifyConnection: func(tls.ConnectionState) error { return original }}, expected)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}), original) {
		t.Fatal("existing TLS verification bypassed")
	}
	if _, err = pinPeer(nil, expected); err == nil {
		t.Fatal("plaintext fallback accepted")
	}
	if _, err = pinPeer(&tls.Config{}, "invalid"); err == nil {
		t.Fatal("invalid peer fingerprint accepted")
	}
}
