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
	for _, want := range []string{
		`MANYFORGE_ENVIRONMENT: "production"`,
		`MANYFORGE_SMTP_HOST: "smtp.example.test"`,
		`MANYFORGE_SMTP_PORT: "2525"`,
		"name: MANYFORGE_SMTP_USER",
		"name: MANYFORGE_SMTP_PASS",
		"name: mailing-smtp",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("rendered chart missing %q\n%s", want, output)
		}
	}
}
