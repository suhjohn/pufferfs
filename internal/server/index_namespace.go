package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func rootIndexNamespaceName(orgID, rootID string) string {
	return fmt.Sprintf("pfs_%s_%s_s000", shortNamespaceHash(orgID), shortNamespaceHash(rootID))
}

func shortNamespaceHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}
