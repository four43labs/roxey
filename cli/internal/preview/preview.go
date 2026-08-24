// Package preview derives fork-preview identities: when `roxey up` runs
// inside a linked git worktree (or with --preview), environment hosts are
// flattened into unique single-label names like
//
//	issue-012-auth-fix-api-verifycate   (+ "." + tld)
//
// so each fork gets its own URLs under the shared relay without touching
// the main checkout's routes.
package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strings"

	"roxey/internal/manifest"
)

// Identity describes one preview instance.
type Identity struct {
	Name   string // registry name: "<project>--<slug>"
	Slug   string // DNS-safe branch/custom slug
	Branch string // original git branch, when detected
}

// Detect reports whether dir is a linked git worktree, returning the current
// branch. ok=false for the main checkout or non-git directories.
func Detect(dir string) (branch string, ok bool) {
	gitDir := run(dir, "git", "rev-parse", "--git-dir")
	common := run(dir, "git", "rev-parse", "--git-common-dir")
	if gitDir == "" || common == "" || clean(gitDir) == clean(common) {
		return "", false // main checkout or not a repo
	}
	branch = run(dir, "git", "branch", "--show-current")
	if branch == "" {
		detached := run(dir, "git", "rev-parse", "--short", "HEAD")
		branch = detached
	}
	return branch, branch != ""
}

// Slugify converts a branch name or custom string into a DNS-safe label:
// lowercase, [a-z0-9-] only, ≤40 chars with a short hash appended on
// truncation so distinct long branches stay distinct.
func Slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "preview"
	}
	if len(out) > 40 {
		sum := sha256.Sum256([]byte(s))
		out = out[:40] + "-" + hex.EncodeToString(sum[:])[:6]
	}
	return out
}

// Apply rewrites m in place into its preview variant: every environment host
// becomes "<slug>-<host with dots as dashes>" (single DNS label), templates
// keep resolving against original names, and the registry name becomes
// "<projectName>--<slug>". Returns the new project name.
func Apply(m *manifest.Manifest, projectName, slug string) string {
	// Original declared hosts (keys), so {{host:X}} keeps working.
	origins := make([]string, 0, len(m.HostMap))
	for orig := range m.HostMap {
		origins = append(origins, orig)
	}

	for i := range m.Environments {
		e := &m.Environments[i]
		e.Host = slug + "-" + strings.ReplaceAll(e.Host, ".", "-")
	}

	for _, orig := range origins {
		m.HostMap[orig] = slug + "-" + strings.ReplaceAll(orig, ".", "-")
	}

	return projectName + "--" + slug
}

func clean(p string) string {
	return strings.TrimSpace(strings.TrimPrefix(p, "./"))
}

func run(dir string, args ...string) string {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
