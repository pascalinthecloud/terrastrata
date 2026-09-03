package modules

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signed archive URLs.
//
// The archive endpoint cannot sit behind bearer auth: Terraform attaches
// registry credentials only to registry endpoints, and the go-getter fetch of
// X-Terraform-Get that follows carries no Authorization header. Without a second
// mechanism that leaves AUTH_TOKEN covering every route but this one, so anyone
// who can reach the port could pull cached module archives and drive upstream
// fetches unauthenticated.
//
// So the URL itself carries the authorization: the download endpoint — which
// *is* behind auth — mints a short-lived HMAC over the module coordinates, and
// the archive endpoint verifies it. Only an authenticated client can obtain a
// usable archive link, and go-getter carries the query string through unchanged.
//
// Signing is keyed on AUTH_TOKEN, so it is active exactly when auth is: with no
// token configured the archive endpoint stays open, which is the default
// internal-network mode.

// archiveURLTTL is how long a minted archive URL stays valid. Terraform fetches
// the archive immediately after resolving the download endpoint, so this only
// has to cover one init; it is not a session lifetime.
const archiveURLTTL = 15 * time.Minute

// Query parameter names carrying the signature on an archive URL.
const (
	paramExpiry    = "exp"
	paramSignature = "sig"
)

// errUnsigned reports a missing, malformed, expired, or incorrect signature on
// an archive request. Handlers map it to a 403.
var errUnsigned = errors.New("modules: archive URL is not validly signed")

// signer mints and verifies archive-URL signatures. A nil *signer means signing
// is disabled (no AUTH_TOKEN), in which case archive URLs carry no signature and
// none is required.
type signer struct {
	key []byte
}

// newSigner returns a signer for key, or nil when key is empty.
func newSigner(key string) *signer {
	if key == "" {
		return nil
	}
	return &signer{key: []byte(key)}
}

// sign returns the hex HMAC-SHA256 binding c to an expiry.
func (s *signer) sign(c Coordinates, expiry int64) string {
	mac := hmac.New(sha256.New, s.key)
	// A domain prefix and a field separator that cannot appear inside a
	// validated coordinate keep the signed message unambiguous.
	_, _ = mac.Write([]byte(strings.Join([]string{
		"terrastrata-module-archive-v1",
		c.Namespace, c.Name, c.System, c.Version,
		strconv.FormatInt(expiry, 10),
	}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

// query returns the signature parameters for c, valid from now.
func (s *signer) query(c Coordinates, now time.Time) string {
	expiry := now.Add(archiveURLTTL).Unix()
	return fmt.Sprintf("&%s=%d&%s=%s", paramExpiry, expiry, paramSignature, s.sign(c, expiry))
}

// verify checks the expiry and signature presented on an archive request. The
// comparison is constant-time, and the expiry is checked first so a stale link
// never reaches the MAC comparison at all.
func (s *signer) verify(c Coordinates, rawExpiry, sig string, now time.Time) error {
	expiry, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil {
		return errUnsigned
	}
	if now.Unix() > expiry {
		return errUnsigned
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(c, expiry))) != 1 {
		return errUnsigned
	}
	return nil
}
