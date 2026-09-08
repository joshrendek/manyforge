package manyforge_test

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOutboundProviderEnvironmentParity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		secret   bool
		disabled bool
	}{
		{"smtp", "smtp", true, false},
		{"resend-without-smtp", "resend", true, false},
		{"ses-without-smtp", "ses", true, false},
		{"no-secret-reference", "resend", false, false},
		{"disabled-without-secret-dependency", "ses", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{
				"outboundMail.provider":            tc.provider,
				"outboundMail.fromEmail":           "accounts@example.test",
				"outboundMail.fromName":            "ManyForge Accounts",
				"outboundMail.sesRegion":           "us-east-1",
				"outboundMail.sesConfigurationSet": "account-mail",
			}
			if tc.provider == "smtp" {
				settings["smtp.host"] = "smtp.example.test"
				settings["smtp.port"] = "2525"
			}
			if tc.secret {
				// Configuring both names must not require the unselected backend's keys.
				settings["secrets.smtp.secretName"] = "smtp-credentials"
				settings["secrets.outboundMail.secretName"] = "api-credentials"
				settings["secrets.outboundMail.resendAPIKeyKey"] = "custom-resend-key"
				settings["secrets.outboundMail.sesAccessKeyIDKey"] = "custom-access-key"
				settings["secrets.outboundMail.sesSecretAccessKeyKey"] = "custom-secret-key"
			}
			if tc.disabled {
				settings["outboundMailDisabled"] = "true"
			}
			output := renderChart(t, settings)
			var configMap struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.Unmarshal([]byte(renderedSource(t, output, "templates/configmap.yaml")), &configMap); err != nil {
				t.Fatal(err)
			}
			app := workloadEnv(t, renderedSource(t, output, "templates/deployment.yaml"), configMap.Data)
			migration := workloadEnv(t, renderedSource(t, output, "templates/migrate-job.yaml"), nil)
			wantConfig := map[string]string{
				"MANYFORGE_ENVIRONMENT":                    "production",
				"MANYFORGE_OUTBOUND_MAIL_DISABLED":         "false",
				"MANYFORGE_OUTBOUND_PROVIDER":              tc.provider,
				"MANYFORGE_OUTBOUND_FROM_EMAIL":            "accounts@example.test",
				"MANYFORGE_OUTBOUND_FROM_NAME":             "ManyForge Accounts",
				"MANYFORGE_OUTBOUND_SES_REGION":            "us-east-1",
				"MANYFORGE_OUTBOUND_SES_CONFIGURATION_SET": "account-mail",
				"MANYFORGE_SMTP_HOST":                      "",
				"MANYFORGE_SMTP_PORT":                      "587",
			}
			if tc.provider == "smtp" {
				wantConfig["MANYFORGE_SMTP_HOST"] = "smtp.example.test"
				wantConfig["MANYFORGE_SMTP_PORT"] = "2525"
			}
			if tc.disabled {
				wantConfig["MANYFORGE_OUTBOUND_MAIL_DISABLED"] = "true"
			}
			for key, value := range wantConfig {
				if app[key].Value != value || migration[key].Value != value {
					t.Fatalf("%s: app=%q migration=%q want=%q", key, app[key].Value, migration[key].Value, value)
				}
			}
			wantSecrets := map[string]chartSecretRef{}
			if tc.secret && !tc.disabled {
				switch tc.provider {
				case "smtp":
					wantSecrets["MANYFORGE_SMTP_USER"] = chartSecretRef{"smtp-credentials", "user"}
					wantSecrets["MANYFORGE_SMTP_PASS"] = chartSecretRef{"smtp-credentials", "pass"}
				case "resend":
					wantSecrets["MANYFORGE_OUTBOUND_RESEND_API_KEY"] = chartSecretRef{"api-credentials", "custom-resend-key"}
				case "ses":
					wantSecrets["MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID"] = chartSecretRef{"api-credentials", "custom-access-key"}
					wantSecrets["MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY"] = chartSecretRef{"api-credentials", "custom-secret-key"}
				}
			}
			for _, key := range []string{
				"MANYFORGE_SMTP_USER", "MANYFORGE_SMTP_PASS", "MANYFORGE_OUTBOUND_RESEND_API_KEY",
				"MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID", "MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY",
			} {
				if _, exists := configMap.Data[key]; exists {
					t.Fatalf("credential %s must never appear in the ConfigMap", key)
				}
				want, expected := wantSecrets[key]
				for name, env := range map[string]map[string]chartEnv{"app": app, "migration": migration} {
					got, exists := env[key]
					if exists != expected || (expected && (got.Value != "" || got.ValueFrom.SecretKeyRef == nil || *got.ValueFrom.SecretKeyRef != want)) {
						t.Fatalf("%s credential %s: got=%+v exists=%v want=%+v expected=%v", name, key, got, exists, want, expected)
					}
				}
			}
		})
	}
}

type chartSecretRef struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type chartEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom struct {
		SecretKeyRef *chartSecretRef `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

func workloadEnv(t *testing.T, document string, config map[string]string) map[string]chartEnv {
	t.Helper()
	var workload struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env     []chartEnv `yaml:"env"`
						EnvFrom []struct {
							ConfigMapRef *struct {
								Name string `yaml:"name"`
							} `yaml:"configMapRef"`
						} `yaml:"envFrom"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(document), &workload); err != nil {
		t.Fatal(err)
	}
	env := make(map[string]chartEnv)
	for _, container := range workload.Spec.Template.Spec.Containers {
		for _, source := range container.EnvFrom {
			if source.ConfigMapRef != nil && source.ConfigMapRef.Name == "mailing-manyforge" {
				for key, value := range config {
					env[key] = chartEnv{Name: key, Value: value}
				}
			}
		}
		for _, value := range container.Env {
			env[value.Name] = value
		}
	}
	return env
}

func renderChart(t *testing.T, settings map[string]string) string {
	t.Helper()
	args := []string{"template", "mailing", ".", "--set", "jwt.activeKid=test-key", "--set", "trustedProxyCidr=10.0.0.0/8"}
	for key, value := range settings {
		args = append(args, "--set", key+"="+value)
	}
	cmd := exec.Command("helm", args...)
	cmd.Dir = "."
	rendered, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, rendered)
	}
	return string(rendered)
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
