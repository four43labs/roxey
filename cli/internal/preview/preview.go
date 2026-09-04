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
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
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
		branch = detachedSlug(detached, dir)
	}
	return branch, branch != ""
}

// PathSlug returns a stable preview identity for a project checkout. It is
// used when hosted mode needs dotted service names flattened outside a linked
// worktree.
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
