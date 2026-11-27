package manyforge_test

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAppAndMigrationImageParity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, digest, want string }{
		{"tag-compatibility", "", "registry.example.test/manyforge:release"},
		{"digest-precedes-tag", digest, "registry.example.test/manyforge@" + digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := renderChart(t, map[string]string{"image.repository": "registry.example.test/manyforge", "image.tag": "release", "image.digest": tc.digest})
			for _, source := range []string{"templates/deployment.yaml", "templates/migrate-job.yaml"} {
				var workload struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Image string `yaml:"image"`
								} `yaml:"containers"`
							} `yaml:"spec"`
						} `yaml:"template"`
					} `yaml:"spec"`
				}
				if err := yaml.Unmarshal([]byte(renderedSource(t, output, source)), &workload); err != nil {
					t.Fatal(err)
				}
				if got := workload.Spec.Template.Spec.Containers; len(got) != 1 || got[0].Image != tc.want {
					t.Fatalf("%s image = %+v, want %s", source, got, tc.want)
				}
			}
		})
	}
	for _, digest := range []string{"latest", "sha256:abc", "sha256:" + strings.Repeat("A", 64)} {
		cmd := exec.Command("helm", "template", "invalid", ".", "--set", "jwt.activeKid=test-key", "--set", "trustedProxyCidr=127.0.0.1/32", "--set-string", "image.digest="+digest)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "image.digest must be a sha256 digest") {
			t.Fatalf("invalid digest accepted or wrong error: %v %s", err, output)
		}
	}
}

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

// Spec 017 R16: the reply-token secret comes from the master-keys Secret (never
// the ConfigMap), MANYFORGE_BLOB_URL is plain config, and optional S3 credentials
// come from a named Secret only when one is configured.
func TestSupportEmailSecretAndBlobWiring(t *testing.T) {
	for _, tc := range []struct {
		name       string
		blobSecret bool
	}{
		{"with-blob-credentials", true},
		{"without-blob-credentials", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"blob.url": "s3://support-attachments"}
			if tc.blobSecret {
				settings["secrets.blob.secretName"] = "blob-credentials"
			}
			output := renderChart(t, settings)
			var configMap struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.Unmarshal([]byte(renderedSource(t, output, "templates/configmap.yaml")), &configMap); err != nil {
				t.Fatal(err)
			}
			if got := configMap.Data["MANYFORGE_BLOB_URL"]; got != "s3://support-attachments" {
				t.Fatalf("MANYFORGE_BLOB_URL = %q, want s3://support-attachments", got)
			}
			for _, key := range []string{"MANYFORGE_INBOUND_REPLY_TOKEN_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
				if _, exists := configMap.Data[key]; exists {
					t.Fatalf("credential %s must never appear in the ConfigMap", key)
				}
			}
			app := workloadEnv(t, renderedSource(t, output, "templates/deployment.yaml"), configMap.Data)
			reply := app["MANYFORGE_INBOUND_REPLY_TOKEN_SECRET"]
			if reply.Value != "" || reply.ValueFrom.SecretKeyRef == nil ||
				*reply.ValueFrom.SecretKeyRef != (chartSecretRef{"manyforge-masterkeys", "inbound-reply-token"}) {
				t.Fatalf("reply-token env = %+v, want secretKeyRef manyforge-masterkeys/inbound-reply-token", reply)
			}
			for key, want := range map[string]chartSecretRef{
				"AWS_ACCESS_KEY_ID":     {"blob-credentials", "access-key-id"},
				"AWS_SECRET_ACCESS_KEY": {"blob-credentials", "secret-access-key"},
			} {
				got, exists := app[key]
				if exists != tc.blobSecret {
					t.Fatalf("%s present = %v, want %v", key, exists, tc.blobSecret)
				}
				if exists && (got.ValueFrom.SecretKeyRef == nil || *got.ValueFrom.SecretKeyRef != want) {
					t.Fatalf("%s = %+v, want secretKeyRef %+v", key, got, want)
				}
			}
		})
	}
}

// renderChartFails renders the chart with the given --set pairs and returns
// the combined output, failing the test if the render unexpectedly succeeds.
func renderChartFails(t *testing.T, settings ...string) string {
	t.Helper()
	args := []string{"template", "mailing", ".", "--set", "jwt.activeKid=test-key", "--set", "trustedProxyCidr=10.0.0.0/8"}
	for _, setting := range settings {
		args = append(args, "--set", setting)
	}
	cmd := exec.Command("helm", args...)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helm template succeeded, want failure for %v\n%s", settings, out)
	}
	return string(out)
}

// Portal custom domains: extra hostnames are extra rules on the SAME Ingress so
// its annotations (source-range restriction, /metrics denial) cover them all.
func TestIngressExtraHosts(t *testing.T) {
	hostRule := func(host string) string { return "- host: \"" + host + "\"" }

	def := renderedSource(t, renderChart(t, nil), "templates/ingress.yaml")
	if got := strings.Count(def, "- host:"); got != 1 {
		t.Fatalf("default render has %d host rules, want 1:\n%s", got, def)
	}

	out := renderChart(t, map[string]string{
		"publicBaseURL":                          "https://app.example.test",
		"ingress.extraHosts":                     "{portal.example.test,Help.Example.Test}",
		"analytics.cloudflareSourceRanges":       "203.0.113.0/24",
		"analytics.trustedIngressNamespace":      "ingress",
		"analytics.trustCloudflareCountryHeader": "true",
	})
	if got := strings.Count(out, "kind: Ingress"); got != 1 {
		t.Fatalf("rendered %d Ingress objects, want exactly 1", got)
	}
	ing := renderedSource(t, out, "templates/ingress.yaml")
	if got := strings.Count(ing, "- host:"); got != 3 {
		t.Fatalf("got %d host rules, want 3:\n%s", got, ing)
	}
	for _, host := range []string{"portal.example.test", "help.example.test"} {
		if !strings.Contains(ing, hostRule(host)) {
			t.Errorf("ingress missing rule for %s", host)
		}
	}
	if got := strings.Count(ing, "service:\n"); got != 3 {
		t.Errorf("got %d backends, want 3", got)
	}
	var doc struct {
		Spec struct {
			Rules []struct {
				HTTP struct {
					Paths []struct {
						Backend struct {
							Service struct {
								Name string `yaml:"name"`
								Port struct {
									Number int `yaml:"number"`
								} `yaml:"port"`
							} `yaml:"service"`
						} `yaml:"backend"`
					} `yaml:"paths"`
				} `yaml:"http"`
			} `yaml:"rules"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(ing), &doc); err != nil {
		t.Fatalf("parse ingress: %v", err)
	}
	first := doc.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	for i, rule := range doc.Spec.Rules {
		if len(rule.HTTP.Paths) != 1 || rule.HTTP.Paths[0].Backend.Service != first {
			t.Errorf("rule %d backend %+v differs from primary %+v", i, rule.HTTP.Paths, first)
		}
	}
	for _, once := range []string{"whitelist-source-range:", "location /metrics"} {
		if got := strings.Count(ing, once); got != 1 {
			t.Errorf("%q appears %d times, want 1", once, got)
		}
	}

	const base = "publicBaseURL=https://app.example.test"
	if msg := renderChartFails(t, base, "ingress.host=hub.example.test", "ingress.extraHosts={HUB.example.test}"); !strings.Contains(msg, "extraHosts") || !strings.Contains(msg, "duplicates") {
		t.Errorf("duplicate-of-host failure should mention extraHosts: %s", msg)
	}
	if msg := renderChartFails(t, base, "ingress.extraHosts={a.example.test,a.example.test}"); !strings.Contains(msg, "extraHosts") || !strings.Contains(msg, "duplicates") {
		t.Errorf("duplicate-entry failure should mention extraHosts: %s", msg)
	}
}

// Every extra hostname must be a plain lower-case FQDN: an empty entry would
// render a catch-all rule, a wildcard would match hosts nobody verified, and a
// port, scheme or space renders a rule that never matches what was meant.
func TestIngressExtraHostsRejectMalformedEntries(t *testing.T) {
	const base = "publicBaseURL=https://app.example.test"
	for _, entries := range []string{
		"{a.example.test,}",
		"{*.example.test}",
		"{portal.example.test:8443}",
		"{https://portal.example.test}",
		"{portal example.test}",
		"{example}",
	} {
		msg := renderChartFails(t, base, "ingress.extraHosts="+entries)
		if !strings.Contains(msg, "extraHosts") || !strings.Contains(msg, "not a plain hostname") {
			t.Errorf("%s: failure should explain the bad extraHosts entry: %s", entries, msg)
		}
	}
}

// Host routing is off without an instance host, so a custom hostname would be
// served the whole operator app: extra hosts need an http(s) publicBaseURL.
func TestIngressExtraHostsRequirePublicBaseURL(t *testing.T) {
	for _, settings := range [][]string{
		{"ingress.extraHosts={portal.example.test}"},
		{"publicBaseURL=app.example.test", "ingress.extraHosts={portal.example.test}"},
		{"publicBaseURL=ftp://app.example.test", "ingress.extraHosts={portal.example.test}"},
	} {
		msg := renderChartFails(t, settings...)
		if !strings.Contains(msg, "extraHosts") || !strings.Contains(msg, "publicBaseURL") {
			t.Errorf("%v: failure should name extraHosts and publicBaseURL: %s", settings, msg)
		}
	}
	// Without extra hosts an empty publicBaseURL is still fine (the default).
	renderChart(t, map[string]string{"ingress.extraHosts": "null"})
}
