// Package nbcount parses OVN external_ids into metric labels and keeps a
// compact per-table row counter.
package nbcount

import (
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	ownerTypeKey       = "k8s.ovn.org/owner-type"
	ownerControllerKey = "k8s.ovn.org/owner-controller"
	controllerSuffix   = "-network-controller"
)

// ParseExternalIDs derives (ownerType, network) from a row's external_ids.
// ownerType is "none" when absent/empty; network is the owner-controller with
// the "-network-controller" suffix stripped, or "default" when absent/empty.
func ParseExternalIDs(m map[string]string) (ownerType, network string) {
	ownerType = m[ownerTypeKey]
	if ownerType == "" {
		ownerType = "none"
	}
	network = strings.TrimSuffix(m[ownerControllerKey], controllerSuffix)
	if network == "" {
		network = "default"
	}
	return ownerType, network
}

// ParseUUID parses a canonical 8-4-4-4-12 hex UUID string.
func ParseUUID(s string) ([16]byte, error) {
	var u [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("invalid uuid %q", s)
	}
	h := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	if _, err := hex.Decode(u[:], []byte(h)); err != nil {
		return [16]byte{}, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	return u, nil
}

// FormatUUID renders u in canonical form.
func FormatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
