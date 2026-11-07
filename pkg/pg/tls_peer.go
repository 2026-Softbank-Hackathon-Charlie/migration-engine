package pg

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TLSPeerSHA256 identifies the endpoint actually observed on a SQL connection.
// PostgreSQL system identifiers are shared by physical replicas and cannot
// distinguish a demoted source from a DSN accidentally redirected to its target.
func (d *DB) TLSPeerSHA256(ctx context.Context) (string, error) {
	c, err := d.Pool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer c.Release()
	t, ok := c.Conn().PgConn().Conn().(*tls.Conn)
	if !ok {
		return "", errors.New("source write fencing requires TLS endpoint identity")
	}
	s := t.ConnectionState()
	if !s.HandshakeComplete || len(s.PeerCertificates) == 0 {
		return "", errors.New("SQL peer certificate is unavailable")
	}
	h := sha256.Sum256(s.PeerCertificates[0].Raw)
	return hex.EncodeToString(h[:]), nil
}

func pinPeer(config *tls.Config, expected string) (*tls.Config, error) {
	if config == nil {
		return nil, errors.New("source write fencing refuses plaintext TLS fallbacks")
	}
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid SQL endpoint fingerprint")
	}
	c := config.Clone()
	original := c.VerifyConnection
	c.VerifyConnection = func(s tls.ConnectionState) error {
		if original != nil {
			if err := original(s); err != nil {
				return err
			}
		}
		if len(s.PeerCertificates) == 0 {
			return errors.New("SQL peer certificate is unavailable")
		}
		h := sha256.Sum256(s.PeerCertificates[0].Raw)
		if hex.EncodeToString(h[:]) != expected {
			return errors.New("SQL endpoint certificate changed during source write fence; reconcile before continuing")
		}
		return nil
	}
	return c, nil
}

// PinTLSPeer preserves existing TLS verification and adds an exact peer check
// to every subsequent connection and TLS fallback. Certificate rotation fails
// closed during this short operation. It does not install or read certificates.
func (d *DB) PinTLSPeer(ctx context.Context, expected string) error {
	cfg := d.Pool.Config()
	var err error
	cfg.ConnConfig.TLSConfig, err = pinPeer(cfg.ConnConfig.TLSConfig, expected)
	if err != nil {
		return err
	}
	for i, fallback := range cfg.ConnConfig.Fallbacks {
		copy := *fallback
		copy.TLSConfig, err = pinPeer(copy.TLSConfig, expected)
		if err != nil {
			return err
		}
		cfg.ConnConfig.Fallbacks[i] = &copy
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return err
	}
	d.Pool.Close()
	d.Pool = pool
	return nil
}
