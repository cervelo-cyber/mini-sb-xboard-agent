package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// ---------------------------------------------------------------------------
// VLESS Reality X25519 keypair management.
//
// Format (verified against sing-box v1.13 source):
//   - private/public keys are 32-byte X25519 keys, base64url without padding
//     (base64.RawURLEncoding). sing-box's own `generate reality-keypair`
//     emits exactly this, and its Reality server parses `private_key` with
//     base64.RawURLEncoding.DecodeString requiring exactly 32 bytes
//     (common/tls/reality_server.go). Xray Reality uses the same encoding.
//   - public_key is always derived from private_key via X25519, so a served
//     pair is never inconsistent.
//
// Generated via Go's standard crypto/ecdh (no third-party dependency), which
// implements the same RFC 7748 X25519 the Reality stack uses.
// ---------------------------------------------------------------------------

// generateRealityKeypair creates a fresh Reality X25519 keypair. It is only
// called on first setup; afterwards the persisted pair is reused forever so
// existing clients keep working across restarts.
func generateRealityKeypair() (privateKey, publicKey string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate reality keypair: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(k.Bytes()),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// deriveRealityPublicKey computes the public key matching a Reality private
// key, or an error if the private key is not a well-formed X25519 key.
func deriveRealityPublicKey(privateKey string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil {
		return "", fmt.Errorf("reality private_key is not valid base64url: %v", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("reality private_key must decode to 32 bytes (got %d)", len(raw))
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("reality private_key is not a valid X25519 key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// validRealityKey reports whether s is a well-formed Reality key (32-byte
// base64url without padding).
func validRealityKey(s string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	return len(raw) == 32
}

// ensureRealityKeypair resolves the Reality keypair of one node entry in
// place, returning whether the entry changed and must be persisted. It only
// applies to nodes that serve VLESS (type vless or both). Behavior:
//
//   - both keys empty: generate a fresh pair (first install);
//   - private present, public missing: derive and store the public key;
//   - both present and consistent: leave untouched;
//   - both present but inconsistent, or public without private: refuse with a
//     clear error instead of serving a broken config or silently changing the
//     node's Reality identity (which would break every existing client).
//
// The private key is generated at most once and reused on later starts.
func ensureRealityKeypair(e *NodeEntry) (changed bool, err error) {
	typ := normalizeNodeType(e.Type)
	if typ != "vless" && typ != "both" {
		return false, nil
	}
	priv, pub := e.TLS.PrivateKey, e.TLS.PublicKey

	if priv == "" && pub == "" {
		pk, pb, err := generateRealityKeypair()
		if err != nil {
			return false, err
		}
		e.TLS.PrivateKey = pk
		e.TLS.PublicKey = pb
		return true, nil
	}

	if priv != "" {
		if !validRealityKey(priv) {
			return false, fmt.Errorf("node %d: reality private_key is not a valid 32-byte base64url X25519 key", e.ID)
		}
		expectedPub, err := deriveRealityPublicKey(priv)
		if err != nil {
			return false, fmt.Errorf("node %d: %v", e.ID, err)
		}
		if pub == "" {
			e.TLS.PublicKey = expectedPub
			return true, nil
		}
		if !validRealityKey(pub) {
			return false, fmt.Errorf("node %d: reality public_key is not a valid 32-byte base64url X25519 key", e.ID)
		}
		if pub != expectedPub {
			return false, fmt.Errorf("node %d: reality private_key and public_key do not match; fix the keypair or delete both keys to force regeneration (auto-replacement is disabled to avoid silently changing the node's Reality identity)", e.ID)
		}
		return false, nil
	}

	return false, fmt.Errorf("node %d: reality public_key is set but private_key is missing; fix the keypair or delete both keys to force regeneration", e.ID)
}
