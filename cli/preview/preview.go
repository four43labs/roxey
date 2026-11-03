// Package preview derives fork-preview identities: when `roxey up` runs
// inside a linked git worktree (or with --preview), environment hosts are
// rewritten so each fork gets its own URLs without touching the main
// checkout's routes.
//
// Preview hosts keep the project's parent domain and add the slug to the
// leftmost label:
//
//	app.acme  ->  app-issue-012-auth-fix.acme  (+ "." + tld)
//	api.acme  ->  api-issue-012-auth-fix.acme
//
// Both stay under the same registrable domain (acme.<tld>), so cookies and
// SameSite=Lax behave exactly as they do on the main checkout. Relays that
// only accept single-label services (the hosted relay) use ModeFlatten,
// which collapses the host into one label instead.
package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/four43labs/roxey/cli/manifest"
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
		branch = detachedSlug(detached, dir)
	}
	return branch, branch != ""
}

// PathSlug returns a stable preview identity for a project checkout. It is
// used when no branch is available, such as --online auto-previews, so two
// checkouts never claim the same hosts.
func PathSlug(projectName, dir string) string {
	return Slugify(projectName + "-" + shortHash(cleanPath(dir)))
}

func detachedSlug(commit, dir string) string {
	return Slugify("detached-" + commit + "-" + filepath.Base(cleanPath(dir)) + "-" + shortHash(cleanPath(dir)))
}

func cleanPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return filepath.Clean(abs)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
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

// Mode selects how Apply derives preview hostnames.
type Mode int

const (
	// ModeSubdomain keeps every host under its existing parent domain by
	// appending the slug to the leftmost label:
	//
	//	app.acme  ->  app-<slug>.acme
	//
	// The result is one extra label, so a "*.acme.<tld>" certificate still
	// covers it, and sibling services stay same-site (shared cookies,
	// SameSite=Lax). Use this for relays that accept dotted service names,
	// including the local relay.
	ModeSubdomain Mode = iota

	// ModeFlatten collapses the whole host into a single label:
	//
	//	app.acme  ->  <slug>-app-acme
	//
	// Hosted relays only accept single-label service names (they append their
	// own account suffix and serve a one-level wildcard), so previews there
	// must flatten.
	ModeFlatten
)

// Apply rewrites m in place into its preview variant using ModeSubdomain.
// It is the right default for local development; callers targeting a relay
// that only accepts single-label services should use ApplyMode with
// ModeFlatten.
func Apply(m *manifest.Manifest, projectName, slug string) string {
	return ApplyMode(m, projectName, slug, ModeSubdomain)
}

// ApplyMode rewrites m in place into its preview variant. Environment hosts
// and the {{host:X}} map are rewritten according to mode; templates keep
// resolving against the original names, and the registry name becomes
// "<projectName>--<slug>". Returns the new project name.
func ApplyMode(m *manifest.Manifest, projectName, slug string, mode Mode) string {
	// Original declared hosts (keys), so {{host:X}} keeps working.
	origins := make([]string, 0, len(m.HostMap))
	for orig := range m.HostMap {
		origins = append(origins, orig)
	}

	for i := range m.Environments {
		m.Environments[i].Host = previewHost(m.Environments[i].Host, slug, mode)
	}
	for _, orig := range origins {
		m.HostMap[orig] = previewHost(orig, slug, mode)
	}

	return projectName + "--" + slug
}

// previewHost derives one preview hostname. Both modes preserve the
// declared host's suffix and stay unique per slug; they differ only in
// whether the slug joins the leftmost label or replaces the label structure.
func previewHost(host, slug string, mode Mode) string {
	if mode == ModeFlatten {
		return slug + "-" + strings.ReplaceAll(host, ".", "-")
	}
	label, rest, dotted := strings.Cut(host, ".")
	label = strings.TrimSuffix(label, "-") + "-" + slug
	if dotted {
		return label + "." + rest
	}
	return label
}

// AssignPorts gives every auto-port route a distinct free port. In preview
// mode it also remaps explicit run-route ports and local proxy aliases that
// target those declared ports.
func AssignPorts(m *manifest.Manifest, isPreview bool, allocate func() (int, error)) error {
	used := map[int]bool{}
	if !isPreview {
		for i := range m.Environments {
			for j := range m.Environments[i].Routes {
				r := &m.Environments[i].Routes[j]
				if r.IsRun() && r.Port > 0 {
					used[r.Port] = true
				}
			}
		}
	}

	oldPorts := map[int]int{}
	ambiguous := map[int]bool{}
	for i := range m.Environments {
		for j := range m.Environments[i].Routes {
			r := &m.Environments[i].Routes[j]
			if !r.IsRun() || (!isPreview && r.Port > 0) {
				continue
			}
			old := r.Port
			port, err := distinctPort(used, allocate)
			if err != nil {
				return fmt.Errorf("assign free port for %s%s: %w", m.Environments[i].Host, r.Path, err)
			}
			r.Port = port
			if isPreview && old > 0 {
				if _, exists := oldPorts[old]; exists {
					ambiguous[old] = true
				} else {
					oldPorts[old] = port
				}
			}
		}
	}

	if !isPreview {
		return nil
	}
	for i := range m.Environments {
		for j := range m.Environments[i].Routes {
			r := &m.Environments[i].Routes[j]
			if r.IsRun() {
				continue
			}
			old, ok := localTargetPort(r.Target)
			if !ok {
				continue
			}
			port, spawned := oldPorts[old]
			if !spawned {
				continue
			}
			if ambiguous[old] {
				return fmt.Errorf("proxy target %q is ambiguous: multiple spawned routes declared port %d", r.Target, old)
			}
			r.Target = rewriteTargetPort(r.Target, port)
		}
	}
	return nil
}

func distinctPort(used map[int]bool, allocate func() (int, error)) (int, error) {
	for {
		port, err := allocate()
		if err != nil {
			return 0, err
		}
		if port <= 0 || port > 65535 {
			return 0, fmt.Errorf("allocator returned invalid port %d", port)
		}
		if used[port] {
			continue
		}
		used[port] = true
		return port, nil
	}
}

func localTargetPort(target string) (int, bool) {
	hostport := target
	if u, err := url.Parse(target); err == nil && strings.Contains(target, "://") {
		hostport = u.Host
	}
	host, rawPort, err := net.SplitHostPort(hostport)
	if err != nil || (host != "localhost" && host != "127.0.0.1") {
		return 0, false
	}
	port, err := strconv.Atoi(rawPort)
	return port, err == nil
}

func rewriteTargetPort(target string, port int) string {
	if u, err := url.Parse(target); err == nil && strings.Contains(target, "://") {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
		return u.String()
	}
	host, _, _ := net.SplitHostPort(target)
	return net.JoinHostPort(host, strconv.Itoa(port))
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
