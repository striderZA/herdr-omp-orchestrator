package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Digest is the lowercase SHA-256 of the facts a user is shown for an inbox note:
// its task, kind, summary, label, delivery state and detail. A settlement quotes
// it back, so the decision applies only to the note exactly as it was inspected.
// Each fact is one JSON element, so two notes whose fields run together to the
// same text still have different digests.
func (i Inbox) Digest() string {
	// Marshalling a slice of strings cannot fail.
	encoded, _ := json.Marshal([]string{i.ID, i.TaskID, i.Kind, i.Summary, i.Delivery, i.State(), i.DeliveryDetail})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
