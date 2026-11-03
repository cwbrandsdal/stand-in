package insights

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"strings"
)

const (
	// viewerKeyEnv holds the hex SHA-256 of the read-only insights key. Only the hash is
	// kept in the process environment; the key itself lives in the operator's secret store.
	viewerKeyEnv = "INSIGHTS_VIEWER_KEY_SHA256"
	// viewerKeyMinLength rejects short keys: an unsalted hash is only safe for random tokens.
	viewerKeyMinLength = 32
)

// ViewerAccessContextKey is set to true on insights requests authenticated with the
// read-only viewer key rather than the management key.
const ViewerAccessContextKey = "insightsViewerAccess"

// ViewerKeyMatches reports whether provided is the read-only insights key. The key opens
// the insights endpoints only; every other management route still needs the management key.
func ViewerKeyMatches(provided string) bool {
	if len(provided) < viewerKeyMinLength {
		return false
	}
	want, errDecode := hex.DecodeString(strings.TrimSpace(os.Getenv(viewerKeyEnv)))
	if errDecode != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}
