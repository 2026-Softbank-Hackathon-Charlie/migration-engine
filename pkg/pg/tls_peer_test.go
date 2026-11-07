package pg

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestPinnedObserverCannotInheritApplicationSearchPath(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://observer:fixture@127.0.0.1/app?sslmode=require&search_path=public,pg_catalog")
	if err != nil {
		t.Fatal(err)
	}
	if err = pinPoolConfig(cfg, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams["search_path"] != "pg_catalog" {
		t.Fatal("application schema can shadow observer catalog functions")
	}
}

// A wrong peer must be rejected during TLS, before PostgreSQL StartupMessage or
// a password challenge. Repinning an already authenticated pool fails this.
func TestConnectPinnedRejectsPeerBeforeSQLAuthentication(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	startupBytes := make(chan int, 1)
	go func() {
		raw, e := listener.Accept()
		if e != nil {
			startupBytes <- -1
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		var sslRequest [8]byte
		if _, e = io.ReadFull(raw, sslRequest[:]); e != nil || binary.BigEndian.Uint32(sslRequest[4:]) != 80877103 {
			startupBytes <- -1
			return
		}
		if _, e = raw.Write([]byte{'S'}); e != nil {
			startupBytes <- -1
			return
		}
		secure := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if secure.Handshake() != nil {
			startupBytes <- 0
			return
		}
		var startup [4]byte
		n, _ := io.ReadFull(secure, startup[:])
		startupBytes <- n
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dsn := fmt.Sprintf("postgres://observer:fixture-only@%s/app?sslmode=require", listener.Addr())
	db, err := ConnectPinned(ctx, dsn, strings.Repeat("0", 64))
	if db != nil {
		db.Close()
	}
	if err == nil {
		t.Fatal("wrong TLS peer connected")
	}
	select {
	case n := <-startupBytes:
		if n != 0 {
			t.Fatalf("SQL startup/authentication began before peer pin: %d bytes", n)
		}
	case <-ctx.Done():
		t.Fatal("peer rejection fixture did not finish")
	}
}
