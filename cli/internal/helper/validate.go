package helper

import (
	"fmt"
	"regexp"
	"strings"
)

// hostLabelRE matches one DNS label: 1-63 chars, alphanumeric with interior
// hyphens, never starting or ending with a hyphen.
var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const maxHosts = 5000

// ValidHostname reports whether h is a syntactically safe lowercase DNS name.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// ValidateHosts rejects any entry that is not a safe hostname tied to one of
// the allowed TLDs. This is the daemon's last line of defence: the client
// cannot inject arbitrary names (or paths/whitespace) into /etc/hosts.
func ValidateHosts(entries, allowedTLDs []string) error {
	if len(entries) > maxHosts {
		return fmt.Errorf("too many host entries (%d, max %d)", len(entries), maxHosts)
	}
	allowed := make(map[string]bool, len(allowedTLDs))
	for _, t := range allowedTLDs {
		allowed[t] = true
	}

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e == "" {
			return fmt.Errorf("empty host entry")
		}
		if seen[e] {
			continue
		}
		seen[e] = true

		if e == "localhost" && allowed["localhost"] {
			continue
		}
		if !ValidHostname(e) {
			return fmt.Errorf("invalid host entry %q", e)
		}
		if !hostMatchesAnyTLD(e, allowed) {
			return fmt.Errorf("host %q is not under a registered roxey TLD", e)
		}
	}
	return nil
}

func hostMatchesAnyTLD(h string, allowed map[string]bool) bool {
	for tld := range allowed {
		if h == tld || strings.HasSuffix(h, "."+tld) {
			return true
		}
	}
	return false
}
