package kube

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
)

func testSpec() sandbox.SandboxSpec {
	return sandbox.SandboxSpec{
		Image: "ghcr.io/manyforge/opencode-sandbox:latest",
		Env: map[string]string{
			"LLM_API_KEY":  "sk-super-secret",
			"LLM_BASE_URL": "https://openrouter.ai/api/v1",
			"LLM_MODEL":    "google/gemini-2.5-pro",
			"LLM_PROVIDER": "openrouter",
		},
		Timeout:           3 * time.Minute,
		Inputs:            map[string][]byte{"review_diff.txt": []byte("diff --git a b")},
		CloneURL:          "https://github.com/example/repo.git",
		CloneAuthHeader:   "AUTHORIZATION: basic dG9rZW4tdmFsdWU=",
		CloneSHA:          "deadbeef",
		CloneAllowPrivate: false,
	}
}

// TestKubeRunner_BuildsHardenedJob pins Task 4.3's hardening requirements: every
// field a security review would check on a Job that runs untrusted, LLM-driven
// code review of a cloned repo.
func TestKubeRunner_BuildsHardenedJob(t *testing.T) {
	r := &KubeRunner{Namespace: "mf-sandbox", ProxyAddr: "http://mf-egress-proxy:8080", PullSecret: "ghcr-auth"}
	spec := testSpec()
	job := r.buildJob("mf-review-abc123", spec, "nonce-value")

	pod := job.Spec.Template.Spec

	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken != false {
		t.Fatalf("automountServiceAccountToken: want false, got %v", pod.AutomountServiceAccountToken)
	}
	sc := pod.SecurityContext
	if sc == nil {
		t.Fatal("pod SecurityContext is nil")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("runAsNonRoot: want true, got %v", sc.RunAsNonRoot)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Fatalf("runAsUser: want 65532, got %v", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 65532 {
		t.Fatalf("runAsGroup: want 65532, got %v", sc.RunAsGroup)
	}
	if sc.FSGroup == nil || *sc.FSGroup != 65532 {
		t.Fatalf("fsGroup: want 65532, got %v", sc.FSGroup)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccompProfile: want RuntimeDefault, got %+v", sc.SeccompProfile)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "ghcr-auth" {
		t.Fatalf("imagePullSecrets: want [ghcr-auth], got %+v", pod.ImagePullSecrets)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy: want Never, got %v", pod.RestartPolicy)
	}

	allContainers := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
	if len(allContainers) != 2 {
		t.Fatalf("want 1 init + 1 main container, got %d containers", len(allContainers))
	}
	for _, c := range allContainers {
		csc := c.SecurityContext
		if csc == nil {
			t.Fatalf("container %q: SecurityContext is nil", c.Name)
		}
		if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
			t.Fatalf("container %q: allowPrivilegeEscalation: want false, got %v", c.Name, csc.AllowPrivilegeEscalation)
		}
		if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
			t.Fatalf("container %q: readOnlyRootFilesystem: want true, got %v", c.Name, csc.ReadOnlyRootFilesystem)
		}
		if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
			t.Fatalf("container %q: capabilities.drop: want [ALL], got %+v", c.Name, csc.Capabilities)
		}
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit: want 0, got %v", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Fatalf("ttlSecondsAfterFinished: want 600, got %v", job.Spec.TTLSecondsAfterFinished)
	}
	wantDeadline := int64(spec.Timeout.Seconds()) + 120
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != wantDeadline {
		t.Fatalf("activeDeadlineSeconds: want %d, got %v", wantDeadline, job.Spec.ActiveDeadlineSeconds)
	}

	// Init container: clone command + mounts.
	init := pod.InitContainers[0]
	if init.Name != "clone" {
		t.Fatalf("init container name: want clone, got %q", init.Name)
	}
	if len(init.Command) < 2 || !strings.Contains(init.Command[len(init.Command)-1], "git") {
		t.Fatalf("init container command missing git clone: %+v", init.Command)
	}
	if !strings.Contains(init.Command[len(init.Command)-1], "$CLONE_URL") {
		t.Fatalf("init container command missing $CLONE_URL: %+v", init.Command)
	}
	mountNames := map[string]bool{}
	for _, vm := range init.VolumeMounts {
		mountNames[vm.Name] = true
	}
	if !mountNames[volIn] {
		t.Fatalf("init container missing %q mount: %+v", volIn, init.VolumeMounts)
	}

	// No secret literal values anywhere in the pod spec's env — every credential
	// arrives via a valueFrom (secretKeyRef), never a bare Value.
	secretValues := []string{spec.CloneAuthHeader, spec.Env["LLM_API_KEY"]}
	for _, c := range allContainers {
		for _, ev := range c.Env {
			for _, secretVal := range secretValues {
				if ev.Value == secretVal {
					t.Fatalf("container %q env %q: secret leaked as literal Value", c.Name, ev.Name)
				}
			}
		}
	}

	// The init container's CLONE_AUTH_HEADER must be a secretKeyRef pointing at
	// the per-run Secret, not a literal.
	var foundCloneAuthRef bool
	for _, ev := range init.Env {
		if ev.Name == cloneAuthHeaderKey {
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("init container %s env: want secretKeyRef, got literal Value=%q", cloneAuthHeaderKey, ev.Value)
			}
			if ev.ValueFrom.SecretKeyRef.Name != "mf-review-abc123" {
				t.Fatalf("init container %s secretKeyRef.Name: want mf-review-abc123, got %q", cloneAuthHeaderKey, ev.ValueFrom.SecretKeyRef.Name)
			}
			foundCloneAuthRef = true
		}
	}
	if !foundCloneAuthRef {
		t.Fatal("init container missing CLONE_AUTH_HEADER env")
	}

	// The main container's LLM_* env references the secret via secretKeyRef too.
	main := pod.Containers[0]
	if main.Name != "review" {
		t.Fatalf("main container name: want review, got %q", main.Name)
	}
	llmRefs := 0
	var foundNonce, foundProxy bool
	for _, ev := range main.Env {
		if strings.HasPrefix(ev.Name, "LLM_") {
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil || ev.ValueFrom.SecretKeyRef.Name != "mf-review-abc123" {
				t.Fatalf("main container env %q: want secretKeyRef to mf-review-abc123, got %+v", ev.Name, ev.ValueFrom)
			}
			llmRefs++
		}
		if ev.Name == "MF_MARKER_NONCE" {
			if ev.Value != "nonce-value" {
				t.Fatalf("MF_MARKER_NONCE: want nonce-value, got %q", ev.Value)
			}
			foundNonce = true
		}
		if ev.Name == "HTTPS_PROXY" || ev.Name == "HTTP_PROXY" {
			if ev.Value != "http://mf-egress-proxy:8080" {
				t.Fatalf("%s: want proxy addr, got %q", ev.Name, ev.Value)
			}
			foundProxy = true
		}
	}
	if llmRefs != len(spec.Env) {
		t.Fatalf("main container: want %d LLM_* secretKeyRef envs, got %d", len(spec.Env), llmRefs)
	}
	if !foundNonce {
		t.Fatal("main container missing MF_MARKER_NONCE env")
	}
	if !foundProxy {
		t.Fatal("main container missing HTTPS_PROXY/HTTP_PROXY env")
	}
	// The main container must never get envFrom the whole per-run Secret — that
	// would also hand it CLONE_AUTH_HEADER, which it has no need for.
	if len(main.EnvFrom) != 0 {
		t.Fatalf("main container EnvFrom: want none (least privilege), got %+v", main.EnvFrom)
	}

	// Resources.
	if q := main.Resources.Limits[corev1.ResourceMemory]; q.String() != "2Gi" {
		t.Fatalf("memory limit: want 2Gi, got %v", q.String())
	}
	if q := main.Resources.Limits[corev1.ResourceCPU]; q.String() != "1" {
		t.Fatalf("cpu limit: want 1, got %v", q.String())
	}
	if q := main.Resources.Requests[corev1.ResourceMemory]; q.String() != "512Mi" {
		t.Fatalf("memory request: want 512Mi, got %v", q.String())
	}
	if q := main.Resources.Requests[corev1.ResourceCPU]; q.String() != "250m" {
		t.Fatalf("cpu request: want 250m, got %v", q.String())
	}
}

// TestKubeRunner_PerRunConfigMapCarriesInputs pins that spec.Inputs lands
// verbatim in the ConfigMap's BinaryData for the init container to copy into /out.
func TestKubeRunner_PerRunConfigMapCarriesInputs(t *testing.T) {
	r := &KubeRunner{Namespace: "mf-sandbox"}
	spec := testSpec()
	cm := r.buildConfigMap("mf-review-abc123", spec)

	if cm.Namespace != "mf-sandbox" {
		t.Fatalf("configmap namespace: want mf-sandbox, got %q", cm.Namespace)
	}
	if len(cm.BinaryData) != len(spec.Inputs) {
		t.Fatalf("configmap BinaryData: want %d entries, got %d", len(spec.Inputs), len(cm.BinaryData))
	}
	for k, v := range spec.Inputs {
		if string(cm.BinaryData[k]) != string(v) {
			t.Fatalf("configmap BinaryData[%q]: want %q, got %q", k, v, cm.BinaryData[k])
		}
	}
}

// TestKubeRunner_SecretCarriesCredsOutOfPodSpec pins that the per-run Secret
// (not the Job/pod spec) is where CLONE_AUTH_HEADER and the LLM_* creds live.
func TestKubeRunner_SecretCarriesCredsOutOfPodSpec(t *testing.T) {
	r := &KubeRunner{Namespace: "mf-sandbox"}
	spec := testSpec()
	secret := r.buildSecret("mf-review-abc123", spec)

	if secret.StringData[cloneAuthHeaderKey] != spec.CloneAuthHeader {
		t.Fatalf("secret[%s]: want %q, got %q", cloneAuthHeaderKey, spec.CloneAuthHeader, secret.StringData[cloneAuthHeaderKey])
	}
	for k, v := range spec.Env {
		if secret.StringData[k] != v {
			t.Fatalf("secret[%s]: want %q, got %q", k, v, secret.StringData[k])
		}
	}
}

// TestKubeRunner_UniqueNames pins Task 4.3's M5 requirement: a multi-lane review
// calls Run once per lane, so names must not collide.
func TestKubeRunner_UniqueNames(t *testing.T) {
	r := &KubeRunner{Namespace: "mf-sandbox"}
	spec := testSpec()
	job1 := r.buildJob(runName(), spec, "n1")
	job2 := r.buildJob(runName(), spec, "n2")

	if job1.Name == job2.Name {
		t.Fatalf("buildJob names collided: %q == %q", job1.Name, job2.Name)
	}
	if !strings.HasPrefix(job1.Name, "mf-review-") || !strings.HasPrefix(job2.Name, "mf-review-") {
		t.Fatalf("job names missing mf-review- prefix: %q, %q", job1.Name, job2.Name)
	}
}

// TestKubeRunner_Run_SucceedsWhenJobSucceeds exercises the full wiring — Secret,
// ConfigMap, Job creation, the wait loop, and best-effort cleanup — against a
// fake clientset. A background goroutine flips the Job's status to Succeeded to
// simulate a controller (the fake clientset has no Job controller of its own).
func TestKubeRunner_Run_SucceedsWhenJobSucceeds(t *testing.T) {
	origPoll := pollInterval
	pollInterval = 5 * time.Millisecond
	defer func() { pollInterval = origPoll }()

	cs := fake.NewSimpleClientset()
	r := &KubeRunner{CS: cs, Namespace: "mf-sandbox", ProxyAddr: "http://proxy:8080", PullSecret: "ghcr-auth"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			jobs, err := cs.BatchV1().Jobs("mf-sandbox").List(t.Context(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				job := jobs.Items[0]
				job.Status.Succeeded = 1
				if _, err := cs.BatchV1().Jobs("mf-sandbox").UpdateStatus(t.Context(), &job, metav1.UpdateOptions{}); err == nil {
					return
				}
			}
			select {
			case <-t.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	res, err := r.Run(t.Context(), testSpec())
	<-done
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode: want 0, got %d", res.ExitCode)
	}
	if res.TimedOut {
		t.Fatal("TimedOut: want false")
	}

	// Best-effort cleanup should have deleted the Secret/ConfigMap/Job.
	if jobs, _ := cs.BatchV1().Jobs("mf-sandbox").List(t.Context(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatalf("job not cleaned up: %+v", jobs.Items)
	}
	if cms, _ := cs.CoreV1().ConfigMaps("mf-sandbox").List(t.Context(), metav1.ListOptions{}); len(cms.Items) != 0 {
		t.Fatalf("configmap not cleaned up: %+v", cms.Items)
	}
	if secrets, _ := cs.CoreV1().Secrets("mf-sandbox").List(t.Context(), metav1.ListOptions{}); len(secrets.Items) != 0 {
		t.Fatalf("secret not cleaned up: %+v", secrets.Items)
	}
}

// TestKubeRunner_Run_FailsWhenJobFails pins that a Job reporting Failed yields
// ExitCode 1 with no Go error (mirroring DockerRunner: a non-zero exit is a
// result, not an error).
func TestKubeRunner_Run_FailsWhenJobFails(t *testing.T) {
	origPoll := pollInterval
	pollInterval = 5 * time.Millisecond
	defer func() { pollInterval = origPoll }()

	cs := fake.NewSimpleClientset()
	r := &KubeRunner{CS: cs, Namespace: "mf-sandbox", ProxyAddr: "http://proxy:8080", PullSecret: "ghcr-auth"}

	go func() {
		for {
			jobs, err := cs.BatchV1().Jobs("mf-sandbox").List(t.Context(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				job := jobs.Items[0]
				job.Status.Failed = 1
				if _, err := cs.BatchV1().Jobs("mf-sandbox").UpdateStatus(t.Context(), &job, metav1.UpdateOptions{}); err == nil {
					return
				}
			}
			select {
			case <-t.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	res, err := r.Run(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("ExitCode: want 1, got %d", res.ExitCode)
	}
}
