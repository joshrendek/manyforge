package coding

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// CloneAtSHA clones cloneURL into destDir and checks out sha. The token is passed
// via an in-memory http.extraHeader (-c), never written to disk or the URL.
// destDir must be empty/non-existent. The caller owns destDir's lifecycle.
//
// allowPrivate controls the SSRF guard: when false, private/loopback addresses are
// rejected (safe default for public GitHub). Set true only for on-prem connectors
// whose AllowPrivateBaseURL flag is set.
func CloneAtSHA(ctx context.Context, cloneURL, authHeader, sha, destDir string, allowPrivate bool) error {
	// SSRF guard: resolve and check the clone URL before handing it to git.
	if err := checkCloneURL(cloneURL, allowPrivate); err != nil {
		return err
	}

	// Minimal env: git must not inherit host config, credentials, or secrets.
	gitEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME=" + destDir,
	}

	clone := exec.CommandContext(ctx, "git",
		"-c", "http.extraHeader="+authHeader,
		"-c", "http.followRedirects=false",
		"-c", "credential.helper=",
		"clone", "--no-tags", "--depth", "50", cloneURL, destDir)
	clone.Env = gitEnv
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("coding: git clone failed: %w (%s)", err, string(out))
	}

	// gitEnv for commands that run inside destDir (HOME is valid now).
	innerEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME=" + destDir,
	}

	co := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
	co.Env = innerEnv
	if out, err := co.CombinedOutput(); err != nil {
		// Shallow clone may not contain sha; fetch it then checkout.
		fetch := exec.CommandContext(ctx, "git", "-C", destDir,
			"-c", "http.extraHeader="+authHeader,
			"-c", "http.followRedirects=false",
			"-c", "credential.helper=",
			"fetch", "--depth", "50", "origin", sha)
		fetch.Env = innerEnv
		if fout, ferr := fetch.CombinedOutput(); ferr != nil {
			return fmt.Errorf("coding: git fetch sha failed: %w (%s)", ferr, string(fout))
		}
		co2 := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
		co2.Env = innerEnv
		if out2, err2 := co2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("coding: git checkout failed: %w (%s / %s)", err2, string(out), string(out2))
		}
	}
	return nil
}

// checkCloneURL enforces the SSRF guard for the clone URL. For http/https URLs,
// every resolved IP is checked against the netsafe policy. Non-http schemes
// (e.g. file://) skip the check — they are only reachable on-host and never
// carry user-supplied remote addresses.
func checkCloneURL(cloneURL string, allowPrivate bool) error {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return fmt.Errorf("coding: invalid clone URL: %w", errs.ErrValidation)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil // file:// and other local schemes are not SSRF-relevant
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("coding: clone URL host %q does not resolve: %w", host, errs.ErrValidation)
	}
	opts := netsafe.Options{AllowLoopback: allowPrivate, AllowPrivate: allowPrivate}
	for _, ip := range ips {
		if netsafe.IsBlocked(ip, opts) {
			// Do NOT include host/IP in message to avoid leaking topology.
			return fmt.Errorf("coding: clone URL resolves to a blocked address: %w", errs.ErrValidation)
		}
	}
	return nil
}
