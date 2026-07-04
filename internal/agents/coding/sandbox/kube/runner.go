// Package kube implements sandbox.SandboxRunner by running each code review as a
// hardened Kubernetes Job (the Talos deploy target has no Docker daemon for
// DockerRunner to shell out to). This file (Task 4.3) builds the per-run
// Secret/ConfigMap/Job, submits them, and waits for the Job to reach a terminal
// state. Log streaming + marker-based result extraction land in Task 4.4 — Run
// here returns a minimal SandboxResult (ExitCode/TimedOut only).
package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
)

// cloneAuthHeaderKey is the per-run Secret key (and, on the init container, the
// secretKeyRef'd env var name) carrying the git credential. It is NEVER set as a
// literal env Value anywhere in the Job/pod spec — only via secretKeyRef, so it
// never shows up in `kubectl get job -o yaml` / API audit logs.
const cloneAuthHeaderKey = "CLONE_AUTH_HEADER"

// cloneMarginSeconds accounts for the init container's clone running INSIDE the
// Job (unlike DockerRunner, which clones host-side before the container starts):
// activeDeadlineSeconds must cover clone time + the review timeout.
const cloneMarginSeconds int64 = 120

// jobTTLSecondsAfterFinished lets the Job (and its Pod) linger briefly after
// completion — useful for operator debugging — before Kubernetes garbage-collects
// it. We also delete the Job/ConfigMap/Secret ourselves (best-effort) once Run
// returns, so this is a backstop, not the primary cleanup path.
const jobTTLSecondsAfterFinished int32 = 600

// pollInterval controls how often Run polls Job status while waiting for a
// terminal state. It's a var (not a const) so tests can shrink it.
var pollInterval = 2 * time.Second

// cleanupTimeout bounds the best-effort delete of the per-run Secret/ConfigMap/Job
// once Run is done with them. It uses a background context (not the run's,
// possibly-already-expired, context) so cleanup isn't skipped on timeout.
const cleanupTimeout = 15 * time.Second

const (
	volWork = "work"
	volOut  = "out"
	volTmp  = "tmp"
	volIn   = "in"
)

// cloneScript is the init container's command. It clones spec.CloneURL at
// --depth 50, checks out spec.CloneSHA, then copies the ConfigMap-mounted
// review_*.txt inputs into /out so the main container's entrypoint can read them
// exactly as it does under DockerRunner (which writes them straight into the
// shared OutputDir).
//
// GIT_TERMINAL_PROMPT=0 + http.followRedirects=false are defense-in-depth: the
// clone URL is ALREADY validated host-side (checkCloneURL, SSRF guard) before
// this Job is ever created (M1), so this is a second layer, not the only guard.
//
// CLONE_AUTH_HEADER is used as-is for -c http.extraHeader — it already IS a full
// "Header-Name: value" line (see github.BasicAuthHeader / clone.go's identical
// usage for DockerRunner's host-side clone), so it must NOT be re-wrapped with an
// extra "Authorization: " prefix here.
const cloneScript = `set -eu
GIT_TERMINAL_PROMPT=0 git -c http.followRedirects=false -c http.extraHeader="${CLONE_AUTH_HEADER}" clone --depth 50 "$CLONE_URL" /work && git -C /work checkout "$CLONE_SHA" && cp -r /in/. /out/`

// KubeRunner implements sandbox.SandboxRunner by running each review as a Job.
// CS is kubernetes.Interface (not *kubernetes.Clientset) so tests can inject
// fake.NewSimpleClientset() without a live cluster.
type KubeRunner struct {
	CS         kubernetes.Interface
	Namespace  string
	ProxyAddr  string // e.g. http://mf-egress-proxy:8080, forced via HTTPS_PROXY/HTTP_PROXY
	Image      string // unused: spec.Image is the source of truth for the containers' image
	PullSecret string // e.g. "ghcr-auth"; referenced via pod.spec.imagePullSecrets
}

var _ sandbox.SandboxRunner = (*KubeRunner)(nil)

// NewKubeRunner returns a KubeRunner submitting Jobs to namespace ns, forcing
// egress through proxyAddr, and pulling images with pullSecret.
func NewKubeRunner(cs kubernetes.Interface, ns, proxyAddr, pullSecret string) *KubeRunner {
	return &KubeRunner{CS: cs, Namespace: ns, ProxyAddr: proxyAddr, PullSecret: pullSecret}
}

// Run creates a per-run Secret + ConfigMap + Job, waits for the Job to reach a
// terminal state, and returns a minimal SandboxResult. Task 4.4 replaces the
// result path with log streaming + marker parsing (Outputs/Stderr); for now a
// failed Job simply yields ExitCode 1.
func (r *KubeRunner) Run(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = 5 * time.Minute
	}

	name := runName()
	// MF_MARKER_NONCE: a fresh random value per run, set as a (non-secret) literal
	// env var on the review container. Task 4.4 doesn't need it threaded through
	// here — it can retrieve the exact value later by fetching this same Job
	// object and reading .spec.template.spec.containers[main].env, since the Job
	// name is already the identifier 4.4 needs to locate the run.
	nonce := randHex(16)

	secret := r.buildSecret(name, spec)
	cm := r.buildConfigMap(name, spec)
	job := r.buildJob(name, spec, nonce)

	deadline := spec.Timeout + time.Duration(cloneMarginSeconds)*time.Second
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	if _, err := r.CS.CoreV1().Secrets(r.Namespace).Create(runCtx, secret, metav1.CreateOptions{}); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create secret: %w", err)
	}
	// From here on, always attempt best-effort cleanup of everything we may have
	// created, even if a later create call fails partway through.
	defer r.cleanup(name)

	if _, err := r.CS.CoreV1().ConfigMaps(r.Namespace).Create(runCtx, cm, metav1.CreateOptions{}); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create configmap: %w", err)
	}
	if _, err := r.CS.BatchV1().Jobs(r.Namespace).Create(runCtx, job, metav1.CreateOptions{}); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create job: %w", err)
	}

	succeeded, waitErr := r.waitForJob(runCtx, name)

	res := sandbox.SandboxResult{}
	if err := runCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			res.TimedOut = true
		}
		return res, fmt.Errorf("kube: run %s: %w", name, err)
	}
	if waitErr != nil {
		return res, fmt.Errorf("kube: wait for job %s: %w", name, waitErr)
	}
	if !succeeded {
		res.ExitCode = 1
	}
	return res, nil
}

// waitForJob polls the Job's status until it reports Succeeded or Failed, or ctx
// is done (deadline/cancel — the caller classifies that via ctx.Err()).
func (r *KubeRunner) waitForJob(ctx context.Context, name string) (succeeded bool, err error) {
	for {
		job, getErr := r.CS.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		if job.Status.Succeeded > 0 {
			return true, nil
		}
		if job.Status.Failed > 0 {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(pollInterval):
		}
	}
}

// cleanup best-effort deletes the per-run Secret/ConfigMap/Job. It uses its own
// background context (with a short bound) rather than the run's context, which
// may already be expired/cancelled by the time cleanup runs.
func (r *KubeRunner) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	background := metav1.DeletePropagationBackground
	_ = r.CS.BatchV1().Jobs(r.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background})
	_ = r.CS.CoreV1().ConfigMaps(r.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = r.CS.CoreV1().Secrets(r.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// buildSecret builds the per-run Secret carrying every secret the pod needs:
// the LLM_* credentials from spec.Env, plus the git clone credential. This is
// the ONLY place these values appear — the Job/pod spec references them via
// secretKeyRef, never as literal env values.
func (r *KubeRunner) buildSecret(name string, spec sandbox.SandboxSpec) *corev1.Secret {
	data := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		data[k] = v
	}
	data[cloneAuthHeaderKey] = spec.CloneAuthHeader
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: runLabels(name)},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

// buildConfigMap builds the per-run ConfigMap carrying spec.Inputs (the
// review_*.txt files) so the init container can copy them into /out.
func (r *KubeRunner) buildConfigMap(name string, spec sandbox.SandboxSpec) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: runLabels(name)},
		BinaryData: spec.Inputs,
	}
}

// buildJob builds the hardened Job for one review run. It is a pure function of
// its inputs (no clientset access) so tests can assert every hardening field
// without a live — or even fake — cluster.
func (r *KubeRunner) buildJob(name string, spec sandbox.SandboxSpec, markerNonce string) *batchv1.Job {
	backoffLimit := int32(0)
	ttl := jobTTLSecondsAfterFinished
	activeDeadline := int64(spec.Timeout.Seconds()) + cloneMarginSeconds

	cloneEnv := []corev1.EnvVar{
		{Name: "CLONE_URL", Value: spec.CloneURL},
		{Name: "CLONE_SHA", Value: spec.CloneSHA},
		// HOME under /tmp (writable emptyDir): the root fs is read-only, so git
		// must not try to read/write a default HOME it can't reach.
		{Name: "HOME", Value: "/tmp"},
		{Name: "GIT_CONFIG_GLOBAL", Value: "/dev/null"},
		{Name: "GIT_CONFIG_SYSTEM", Value: "/dev/null"},
		{
			Name: cloneAuthHeaderKey,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Key:                  cloneAuthHeaderKey,
				},
			},
		},
	}

	reviewEnv := []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: r.ProxyAddr},
		{Name: "HTTP_PROXY", Value: r.ProxyAddr},
		{Name: "MF_MARKER_NONCE", Value: markerNonce},
	}
	// LLM_* creds: one secretKeyRef env var per key already present in spec.Env.
	// Deliberately NOT a blanket envFrom of the whole per-run Secret — that would
	// also hand this container CLONE_AUTH_HEADER, which it has no use for (the
	// init container already consumed it to perform the clone). Sorted for
	// deterministic Job specs (easier diffing/testing).
	for _, k := range sortedKeys(spec.Env) {
		reviewEnv = append(reviewEnv, corev1.EnvVar{
			Name: k,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Key:                  k,
				},
			},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: runLabels(name)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: runLabels(name)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					ImagePullSecrets:             []corev1.LocalObjectReference{{Name: r.PullSecret}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(65532),
						RunAsGroup:   int64Ptr(65532),
						FSGroup:      int64Ptr(65532),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Volumes: []corev1.Volume{
						{Name: volWork, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: volOut, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: volTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{
							Name: volIn,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: name},
								},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:    "clone",
							Image:   spec.Image,
							Command: []string{"sh", "-c", cloneScript},
							Env:     cloneEnv,
							VolumeMounts: []corev1.VolumeMount{
								{Name: volWork, MountPath: "/work"},
								{Name: volOut, MountPath: "/out"},
								{Name: volIn, MountPath: "/in", ReadOnly: true},
								{Name: volTmp, MountPath: "/tmp"},
							},
							SecurityContext: nonRootContainerSecurityContext(),
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "review",
							Image: spec.Image,
							Env:   reviewEnv,
							VolumeMounts: []corev1.VolumeMount{
								{Name: volWork, MountPath: "/work", ReadOnly: true},
								{Name: volOut, MountPath: "/out"},
								{Name: volTmp, MountPath: "/tmp"},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("2Gi"),
									corev1.ResourceCPU:    resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("250m"),
								},
							},
							SecurityContext: nonRootContainerSecurityContext(),
						},
					},
				},
			},
		},
	}
	return job
}

// nonRootContainerSecurityContext is shared by both containers: read-only root
// fs, no privilege escalation, every Linux capability dropped.
func nonRootContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func runLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "manyforge",
		"app.kubernetes.io/component":  "sandbox-review",
		"mf.dev/run":                   name,
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runName returns a short, unique, DNS-1123-safe name for one review run, shared
// by its Job/ConfigMap/Secret. A multi-lane review calls Run once per lane, so
// names must not collide even when created back-to-back within the same
// millisecond — crypto/rand (not math/rand) guarantees that regardless of clock
// resolution.
func runName() string {
	return "mf-review-" + randHex(4)
}

// randHex returns n random bytes hex-encoded. crypto/rand.Read on the values used
// here (4 or 16 bytes) cannot practically fail.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
