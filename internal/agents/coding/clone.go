package coding

import (
	"context"
	"fmt"
	"os/exec"
)

// CloneAtSHA clones cloneURL into destDir and checks out sha. The token is passed
// via an in-memory http.extraHeader (-c), never written to disk or the URL.
// destDir must be empty/non-existent. The caller owns destDir's lifecycle.
func CloneAtSHA(ctx context.Context, cloneURL, authHeader, sha, destDir string) error {
	clone := exec.CommandContext(ctx, "git",
		"-c", "http.extraHeader="+authHeader,
		"clone", "--no-tags", "--depth", "50", cloneURL, destDir)
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("coding: git clone failed: %w (%s)", err, string(out))
	}
	co := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
	if out, err := co.CombinedOutput(); err != nil {
		// Shallow clone may not contain sha; fetch it then checkout.
		fetch := exec.CommandContext(ctx, "git", "-C", destDir,
			"-c", "http.extraHeader="+authHeader, "fetch", "--depth", "50", "origin", sha)
		if fout, ferr := fetch.CombinedOutput(); ferr != nil {
			return fmt.Errorf("coding: git fetch sha failed: %w (%s)", ferr, string(fout))
		}
		co2 := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
		if out2, err2 := co2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("coding: git checkout failed: %w (%s / %s)", err2, string(out), string(out2))
		}
	}
	return nil
}
