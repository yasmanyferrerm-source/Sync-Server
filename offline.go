package main

// ---------------------------------------------------------------------------
// Offline pass — lets a signed-in device keep working without the server for
// 7 days. The server signs {user, company, role, expiry} with its own ECDSA
// P-256 key; the app checks the signature (WebCrypto) and the expiry before it
// opens offline. The server still re-checks every change against the user's
// CURRENT role when the device syncs again, so the pass only decides whether the
// app opens, never what the server accepts.
// ---------------------------------------------------------------------------

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	offKeyOnce sync.Once
	offKey     *ecdsa.PrivateKey
)

func offlineKey() *ecdsa.PrivateKey {
	offKeyOnce.Do(func() {
		p := filepath.Join(acc.dir, "offline-key.pem")
		if b, err := os.ReadFile(p); err == nil {
			if blk, _ := pem.Decode(b); blk != nil {
				if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
					if ek, ok := k.(*ecdsa.PrivateKey); ok {
						offKey = ek
						return
					}
				}
			}
		}
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return
		}
		offKey = k
		if der, err := x509.MarshalPKCS8PrivateKey(k); err == nil {
			_ = os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
		}
	})
	return offKey
}

var b64u = base64.RawURLEncoding

// offlinePass returns "v1.<payload>.<signature>" (base64url). The signature (raw r||s, as
// WebCrypto expects) covers the payload text exactly as sent.
func offlinePass(u *user, c *company) string {
	k := offlineKey()
	if k == nil || u == nil {
		return ""
	}
	cname := ""
	if c != nil {
		cname = c.Name
	}
	now := time.Now()
	p, _ := json.Marshal(map[string]any{"uid": u.ID, "cid": u.CompanyID, "role": u.Role, "name": u.Name, "username": u.Username, "company": cname,
		"iat": now.UnixMilli(), "exp": now.Add(offlineDays * 24 * time.Hour).UnixMilli()})
	payload := b64u.EncodeToString(p)
	h := sha256.Sum256([]byte(payload))
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		return ""
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "v1." + payload + "." + b64u.EncodeToString(sig)
}

// offlinePubJWK is the public half, in the JWK form WebCrypto imports.
func offlinePubJWK() map[string]any {
	k := offlineKey()
	if k == nil {
		return nil
	}
	x := make([]byte, 32)
	y := make([]byte, 32)
	k.PublicKey.X.FillBytes(x)
	k.PublicKey.Y.FillBytes(y)
	return map[string]any{"kty": "EC", "crv": "P-256", "x": b64u.EncodeToString(x), "y": b64u.EncodeToString(y)}
}
