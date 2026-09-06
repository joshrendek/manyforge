package manyforge_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestProductionSMTPConfigurationRendersIntoPodEnvironment(t *testing.T) {
	args := []string{
		"template", "mailing", ".",
		"--set", "jwt.activeKid=test-key",
		"--set", "trustedProxyCidr=10.0.0.0/8",
		"--set", "environment=production",
		"--set", "smtp.host=smtp.example.test",
		"--set", "smtp.port=2525",
		"--set", "secrets.smtp.secretName=mailing-smtp",
	}
	cmd := exec.Command("helm", args...)
	cmd.Dir = "."
	rendered, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, rendered)
	}
	output := string(rendered)
	configMap := renderedSource(t, output, "templates/configmap.yaml")
	for _, want := range []string{
		`MANYFORGE_ENVIRONMENT: "production"`,
		`MANYFORGE_SMTP_HOST: "smtp.example.test"`,
		`MANYFORGE_SMTP_PORT: "2525"`,
	} {
		if !strings.Contains(configMap, want) {
			t.Fatalf("rendered ConfigMap missing %q\n%s", want, configMap)
		}
	}
	deployment := renderedSource(t, output, "templates/deployment.yaml")
	for _, want := range []string{
		"envFrom:", "configMapRef:", "name: MANYFORGE_SMTP_USER",
		"name: MANYFORGE_SMTP_PASS", "name: mailing-smtp",
	} {
		if !strings.Contains(deployment, want) {
			t.Fatalf("rendered Deployment missing %q\n%s", want, deployment)
		}
	}
	migrateJob := renderedSource(t, output, "templates/migrate-job.yaml")
	for _, want := range []string{
		"name: MANYFORGE_ENVIRONMENT", `value: "production"`,
		"name: MANYFORGE_SMTP_HOST", `value: "smtp.example.test"`,
		"name: MANYFORGE_SMTP_PORT", `value: "2525"`,
	} {
		if !strings.Contains(migrateJob, want) {
			t.Fatalf("rendered migrate Job missing %q\n%s", want, migrateJob)
		}
	}
}

func renderedSource(t *testing.T, output, source string) string {
	t.Helper()
	marker := "# Source: manyforge/" + source
	start := strings.Index(output, marker)
	if start < 0 {
		t.Fatalf("rendered chart missing source %q", source)
	}
	document := output[start:]
	if end := strings.Index(document, "\n---\n"); end >= 0 {
		document = document[:end]
	}
	return document
}
