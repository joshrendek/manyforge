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
		"trap 'rm -f /tmp/maxmind.netrc' EXIT HUP INT TERM",
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
	if strings.Contains(dockerfile, "ARG GEOIP_DOWNLOAD_URL") {
		t.Error("the credentialed MaxMind endpoint must not be configurable through a build arg")
	}

	appStart := strings.Index(workflow, "      - name: Build and push app image")
	if appStart < 0 {
		t.Fatal("image workflow is missing the app-image build step")
	}
	appBlock := workflow[appStart:]
	if next := strings.Index(appBlock[1:], "\n      - name:"); next >= 0 {
		appBlock = appBlock[:next+1]
	}
	if !strings.Contains(appBlock, "uses: docker/build-push-action@v6") {
		t.Fatal("app-image step must use docker/build-push-action")
	}
	if strings.Contains(appBlock, "GEOIP_TEST_MODE") {
		t.Fatal("the production app-image workflow must never enable the sentinel-only GeoIP test mode")
	}
	secretsStart := strings.Index(appBlock, "\n          secrets: |")
	buildArgsStart := strings.Index(appBlock, "\n          build-args: |")
	if secretsStart < 0 || buildArgsStart <= secretsStart {
		t.Fatal("app-image step must keep BuildKit secrets separate from build args")
	}
	secretsBlock := appBlock[secretsStart:buildArgsStart]
	for _, want := range []string{
		"maxmind_account_id=${{ secrets.MAXMIND_ACCOUNT_ID }}",
		"maxmind_license_key=${{ secrets.MAXMIND_LICENSE_KEY }}",
	} {
		if !strings.Contains(secretsBlock, want) {
			t.Errorf("image workflow must contain BuildKit secret mapping %q", want)
		}
	}

	presenceExpr := "${{ secrets.MAXMIND_ACCOUNT_ID != '' && secrets.MAXMIND_LICENSE_KEY != '' }}"
	outsideSecrets := strings.Replace(appBlock, secretsBlock, "", 1)
	if strings.Count(outsideSecrets, presenceExpr) != 1 {
		t.Fatal("app-image step must use the exact non-secret credential-presence expression once")
	}
	outsideSecrets = strings.Replace(outsideSecrets, presenceExpr, "", 1)
	if strings.Contains(outsideSecrets, "secrets.MAXMIND_") {
		t.Error("MaxMind credential values must appear only in the app step's BuildKit secrets block")
	}
	outsideApp := strings.Replace(workflow, appBlock, "", 1)
	if strings.Contains(outsideApp, "secrets.MAXMIND_") {
		t.Error("MaxMind credentials must not be passed to any workflow step except the app-image build")
	}
}
