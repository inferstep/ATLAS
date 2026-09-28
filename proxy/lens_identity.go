// The proxy's own identity on the Lens call it makes directly.
//
// One proxy-owned call reaches the geometric-lens service and makes it embed
// text with the model server: per-write scoring (/internal/lens/score-per-step).
// It is not a V3 candidate invocation, so it has no V3 invocation identity to
// carry; until now it carried only the request id the outbound transport
// stamps, and an acquisition relay that requires a (request, invocation) pair
// on every embedding refused it.
//
// This file is the single owner of that identity. lensInvocationID derives a
// request-scoped invocation identity from the typed request id and nothing
// else: deterministic (an acquisition runner can register the pair before any
// model-bound traffic), distinct across requests, stable across every direct
// Lens call within one request, of closed format and bounded length, and
// impossible for the model to supply or override (the model never sets request
// headers). It authorises nothing: no policy, permission, grant, obligation,
// candidate or completion owner reads it. It is not a secret. The value travels
// in the historical X-ATLAS-V3-Invocation-ID channel because that is the one
// invocation channel the Lens middleware binds and forwards; the name is
// legacy, the value is a general model-bound invocation identity, and for these
// calls it is a proxy-Lens request scope, never a V3 generation invocation.
// The specification and test vectors live in testdata/lens_invocation_vectors.json.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// lensInvocationHeader is the invocation channel the Lens middleware binds.
// Historical name; see the file comment.
const lensInvocationHeader = "X-ATLAS-V3-Invocation-ID"

// lensInvocationScheme is hashed ahead of the request id so the derived value
// can never collide with another scheme's derivation from the same id.
const lensInvocationScheme = "atlas/proxy-lens-invocation/v1"

// lensInvocationPrefix names the value for what it is.
const lensInvocationPrefix = "proxy-lens:"

const (
	lensRequestIDMaxLen   = 128
	lensInvocationHexLen  = 32
	lensInvocationTotalLe = len(lensInvocationPrefix) + lensInvocationHexLen
)

// validLensRequestID is the closed request-id alphabet the derivation accepts:
// ASCII letters, digits, '.', '_', ':' and '-', 1 to 128 bytes. Minted ids
// (req-<16 hex>) and acquisition case ids satisfy it; anything else carries no
// invocation identity rather than a guessed one.
func validLensRequestID(requestID string) bool {
	if len(requestID) == 0 || len(requestID) > lensRequestIDMaxLen {
		return false
	}
	for i := 0; i < len(requestID); i++ {
		c := requestID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// lensInvocationID derives the proxy-owned Lens invocation identity for a
// request. ok is false when the request id is absent or outside the closed
// alphabet; then the call carries no invocation identity.
func lensInvocationID(requestID string) (id string, ok bool) {
	if !validLensRequestID(requestID) {
		return "", false
	}
	sum := sha256.Sum256([]byte(lensInvocationScheme + "\n" + requestID))
	return lensInvocationPrefix + hex.EncodeToString(sum[:])[:lensInvocationHexLen], true
}

// newLensRequest is the one constructor for proxy-owned model-bound Lens
// calls. It carries the request id bound in ctx and, when derivable, the
// proxy-Lens invocation identity; both headers are set here explicitly so the
// pair does not depend on the outbound transport. A ctx without a request id
// yields a request with neither header: absent work inherits nothing.
func newLensRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if rid := requestIDFromContext(ctx); rid != "" {
		req.Header.Set(requestIDHeader, rid)
		if inv, ok := lensInvocationID(rid); ok {
			req.Header.Set(lensInvocationHeader, inv)
		}
	}
	return req, nil
}
