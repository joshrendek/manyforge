package security_regression

import (
	"strings"
	"testing"
)

// TestMaxMindCredentialsStayInBuildKitSecrets pins the image build's credential boundary.
// MaxMind credentials may be mounted into one RUN instruction, but must never become Docker build
// args or environment variables because those can be retained in image metadata and build history.
func TestMaxMindCredentialsStayInBuildKitSecrets(t *testing.T) {
	dockerfile := mustRead(t, "../../Dockerfile")
	workflow := mustRead(t, "../../.github/workflows/image.yml")

	for _, want := range []string{
		"--mount=type=secret,id=maxmind_account_id",
		"--mount=type=secret,id=maxmind_license_key",
		"--netrc-file /tmp/maxmind.netrc",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile must contain %q", want)
		}
	}
	for _, forbidden := range []string{
		"ARG MAXMIND_ACCOUNT_ID",
		"ARG MAXMIND_LICENSE_KEY",
		"ENV MAXMIND_ACCOUNT_ID",
		"ENV MAXMIND_LICENSE_KEY",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("Dockerfile must not contain %q; credentials belong in BuildKit secrets", forbidden)
		}
	}

	for _, want := range []string{
		"maxmind_account_id=${{ secrets.MAXMIND_ACCOUNT_ID }}",
		"maxmind_license_key=${{ secrets.MAXMIND_LICENSE_KEY }}",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("image workflow must contain BuildKit secret mapping %q", want)
		}
	}
	for _, forbidden := range []string{
		"MAXMIND_ACCOUNT_ID=${{ secrets.MAXMIND_ACCOUNT_ID }}",
		"MAXMIND_LICENSE_KEY=${{ secrets.MAXMIND_LICENSE_KEY }}",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("image workflow must not expose a credential through build-args: %q", forbidden)
		}
	}
}
