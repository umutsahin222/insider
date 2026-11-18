package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
	"time"
)

// Request fingerprints distinguish a retried request (same Idempotency-Key, same body) from a
// reused key (same key, different body). See ADR-006.
//
// A fingerprint covers exactly the fields that decide what gets delivered, after normalisation
// (e.g. an omitted priority and "normal" hash the same). It deliberately excludes the
// correlation ID and the idempotency key itself. The version prefix lets the definition evolve
// without old fingerprints silently comparing equal to new ones.

const (
	notificationFingerprintV1 = "notification/v1"
	batchFingerprintV1        = "batch/v1"
)

// VerifyReplay checks that a request carrying an already-used idempotency key is the same
// request that first used it. storedHash is empty for rows written before fingerprints existed;
// those cannot be verified and are accepted as replays.
func VerifyReplay(storedHash, requestHash string) error {
	if storedHash == "" || storedHash == requestHash {
		return nil
	}
	return &Error{Kind: ErrIdempotencyMismatch, Msg: "Idempotency-Key was already used with a different request body"}
}

func notificationFingerprint(n Notification) string {
	h := sha256.New()
	writeField(h, notificationFingerprintV1)
	writeField(h, string(n.Channel))
	writeField(h, n.Recipient)
	writeField(h, n.Content)
	writeField(h, strconv.Itoa(int(n.Priority)))
	scheduled := ""
	if n.ScheduledAt != nil {
		// Microseconds: the precision PostgreSQL stores, so a value read back hashes the same.
		scheduled = n.ScheduledAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	}
	writeField(h, scheduled)
	template := ""
	if n.TemplateID != nil {
		template = n.TemplateID.String()
	}
	writeField(h, template)
	return hex.EncodeToString(h.Sum(nil))
}

// batchFingerprint covers every item in order, including per-item idempotency keys.
func batchFingerprint(ns []Notification) string {
	h := sha256.New()
	writeField(h, batchFingerprintV1)
	writeField(h, strconv.Itoa(len(ns)))
	for _, n := range ns {
		writeField(h, n.RequestHash)
		key := ""
		if n.IdempotencyKey != nil {
			key = *n.IdempotencyKey
		}
		writeField(h, key)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeField length-prefixes s so that field boundaries are unambiguous ("ab"+"c" ≠ "a"+"bc").
func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}
