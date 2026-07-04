// Package kube implements sandbox.SandboxRunner by running each code review as a
// hardened Kubernetes Job (the Talos deploy target has no Docker daemon for
// DockerRunner to shell out to). Task 4.3 built the per-run Secret/ConfigMap/Job
// and the terminal-state wait; Task 4.4 (this file) adds the result path: Run
// streams the Job pod's logs live (feeding spec.StreamStderr, mirroring
// DockerRunner's progress streaming) and extracts the nonce-marker blocks
// deploy/sandbox/entrypoint.sh writes to stdout into SandboxResult.Outputs.
package kube

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Run creates a per-run Job + Secret + ConfigMap, streams the Job pod's logs
// live while waiting for the Job to reach a terminal state, and returns a
// SandboxResult with Outputs/Stderr extracted from the nonce-marker blocks
// deploy/sandbox/entrypoint.sh writes to stdout.
//
// The Job is created FIRST (before the Secret/ConfigMap) so its UID is known
// and can be stamped onto the Secret/ConfigMap as an ownerReference. That way
// Kubernetes' own garbage collector deletes the credential-bearing Secret and
// ConfigMap when the Job goes away — via cleanup's best-effort delete on the
// happy path, or via the Job's TTLSecondsAfterFinished/deletion regardless of
// this process's lifetime, so a crash/OOM/restart mid-run can no longer
// orphan them permanently. The pod itself won't be able to start until the
// Secret/ConfigMap it references exist a moment later; Kubernetes just
// retries pod creation in the meantime, which is fine.
func (r *KubeRunner) Run(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	spec.Timeout = normalizedTimeout(spec)

	name := runName()
	// MF_MARKER_NONCE: a fresh random value per run, set as a (non-secret) literal
	// env var on the review container (buildJob) AND kept here so Run can parse
	// the Job pod's logs against the exact same value.
	nonce := randHex(16)

	job := r.buildJob(name, spec, nonce)

	deadline := spec.Timeout + time.Duration(cloneMarginSeconds)*time.Second
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	createdJob, err := r.CS.BatchV1().Jobs(r.Namespace).Create(runCtx, job, metav1.CreateOptions{})
	if err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create job: %w", err)
	}
	// From here on, always attempt best-effort cleanup of everything we may have
	// created, even if a later create call fails partway through. This is the
	// happy-path cleanup; the ownerReference above is the crash safety net.
	defer r.cleanup(name)

	owner := ownerReferences(createdJob)
	secret := r.buildSecret(name, spec, owner)
	cm := r.buildConfigMap(name, spec, owner)

	if _, err := r.CS.CoreV1().Secrets(r.Namespace).Create(runCtx, secret, metav1.CreateOptions{}); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create secret: %w", err)
	}
	if _, err := r.CS.CoreV1().ConfigMaps(r.Namespace).Create(runCtx, cm, metav1.CreateOptions{}); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("kube: create configmap: %w", err)
	}

	// Stream the Job pod's logs live: feeds spec.StreamStderr with progress
	// narration as the review runs (mirroring DockerRunner) and best-effort
	// extracts the marker blocks as they appear. This runs concurrently with
	// waitForJob below and is bounded by streamCtx, which we cancel once the Job
	// itself is terminal — so a pod that never appears (e.g. the fake clientset
	// used in unit tests has no real Job controller to create one) can't block
	// Run() past the Job's own terminal state. Best-effort only: the non-follow
	// re-read after the Job is terminal (below) is the authoritative source.
	streamCtx, cancelStream := context.WithCancel(runCtx)
	var followOutputs map[string][]byte
	var followStderr []byte
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		followOutputs, followStderr = r.streamPodLogs(streamCtx, name, nonce, spec.StreamStderr)
	}()

	finalJob, succeeded, waitErr := r.waitForJob(runCtx, name)
	cancelStream()
	<-logDone

	res := sandbox.SandboxResult{Outputs: followOutputs, Stderr: followStderr}
	if err := runCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			res.TimedOut = true
		}
		return res, fmt.Errorf("kube: run %s: %w", name, err)
	}
	if waitErr != nil {
		return res, fmt.Errorf("kube: wait for job %s: %w", name, waitErr)
	}

	// Cross-check (v2 C3): now that the Job is terminal, re-fetch the pod's
	// COMPLETE logs without Follow and re-parse. A log-rotation/race can leave a
	// gap in the follow-stream's view of a fast-finishing pod; the non-follow
	// read sees the full log the kubelet retained, so it wins for any key the
	// follow-stream missed. Best-effort: if the re-read itself fails (e.g. no
	// pod was ever found, as in the fake-clientset unit tests), keep whatever
	// the follow-stream already captured.
	if rereadOutputs, rereadStderr, rerr := r.rereadPodLogs(runCtx, name, nonce); rerr == nil {
		if res.Outputs == nil {
			res.Outputs = map[string][]byte{}
		}
		for k, v := range rereadOutputs {
			if _, ok := res.Outputs[k]; !ok {
				res.Outputs[k] = v
			}
		}
		if len(res.Stderr) == 0 {
			res.Stderr = rereadStderr
		}
	}

	if !succeeded {
		res.ExitCode = 1
		if jobDeadlineExceeded(finalJob) {
			res.TimedOut = true
		}
	}
	return res, nil
}

// waitForJob polls the Job's status until it reports Succeeded or Failed, or ctx
// is done (deadline/cancel — the caller classifies that via ctx.Err()). It
// returns the last-observed Job object so the caller can inspect
// Status.Conditions (jobDeadlineExceeded) without a second Get.
func (r *KubeRunner) waitForJob(ctx context.Context, name string) (job *batchv1.Job, succeeded bool, err error) {
	for {
		j, getErr := r.CS.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, false, getErr
		}
		if j.Status.Succeeded > 0 {
			return j, true, nil
		}
		if j.Status.Failed > 0 {
			return j, false, nil
		}
		select {
		case <-ctx.Done():
			return j, false, nil
		case <-time.After(pollInterval):
		}
	}
}

// jobDeadlineExceeded reports whether job's Failed condition carries the
// "DeadlineExceeded" reason kube-controller-manager sets when a Job runs
// longer than its ActiveDeadlineSeconds — the authoritative, Kubernetes-side
// signal that this specific run timed out (as opposed to Run's own runCtx
// expiring for some unrelated reason, e.g. a slow API call).
func jobDeadlineExceeded(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == "DeadlineExceeded" {
			return true
		}
	}
	return false
}

// findRunPod locates the single Pod the Job created for this run, identified by
// the mf.dev/run label buildJob's pod template stamps on it (a Job-created Pod
// gets an auto-generated name, so we can't just Get by the run name).
func (r *KubeRunner) findRunPod(ctx context.Context, name string) (*corev1.Pod, error) {
	pods, err := r.CS.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "mf.dev/run=" + name,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("kube: no pod found for run %s", name)
	}
	return &pods.Items[0], nil
}

// waitForPodRunningOrTerminal polls until the run's Pod reaches Running OR a
// terminal phase (Succeeded/Failed) — not just Running. A pod can fail fast
// (ImagePullBackOff, the init container's clone failing, …) and go straight
// from Pending to Failed without ever becoming Running; blocking for Running
// only would then hang this goroutine until ctx's deadline even though the Job
// is already done. Bounded by ctx (the caller cancels it once the Job is
// terminal), so a pod that never appears can't block Run() indefinitely.
func (r *KubeRunner) waitForPodRunningOrTerminal(ctx context.Context, name string) (*corev1.Pod, error) {
	for {
		pod, err := r.findRunPod(ctx, name)
		if err == nil {
			switch pod.Status.Phase {
			case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
				return pod, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// streamPodLogs waits for the run's Pod (handling the fast-fail race via
// waitForPodRunningOrTerminal), then follows its logs live and parses out the
// nonce-marker blocks as they arrive so spec.StreamStderr sees progress as the
// review runs. Best-effort: any failure to find the pod or open the log stream
// yields empty results — Run()'s rereadPodLogs, run after the Job is terminal,
// is the authoritative source.
func (r *KubeRunner) streamPodLogs(ctx context.Context, name, nonce string, stderr io.Writer) (map[string][]byte, []byte) {
	pod, err := r.waitForPodRunningOrTerminal(ctx, name)
	if err != nil || pod == nil {
		return nil, nil
	}
	stream, err := r.CS.CoreV1().Pods(r.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = stream.Close() }()
	outputs, stderrTail, _ := parsePodLogs(stream, nonce, stderr)
	return outputs, stderrTail
}

// rereadPodLogs re-fetches the run's Pod logs WITHOUT Follow, once the Job is
// terminal, as the v2 C3 cross-check against the live follow-stream: a log
// rotation, or a race between the follow-stream ending and the container's
// last writes, can leave a gap the full re-read won't have.
func (r *KubeRunner) rereadPodLogs(ctx context.Context, name, nonce string) (map[string][]byte, []byte, error) {
	pod, err := r.findRunPod(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	stream, err := r.CS.CoreV1().Pods(r.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = stream.Close() }()
	return parsePodLogs(stream, nonce, io.Discard)
}

// stderrTailCap bounds the narration tail parsePodLogs retains for
// sandboxStderrTail (M6) diagnostics — mirrors the "short tail" contract
// sandboxStderrTail already documents, so a very chatty opencode run can't
// balloon SandboxResult.Stderr without bound.
const stderrTailCap = 8 * 1024

// maxPodLogLine bounds a single scanned log line. review.json/usage.json are
// base64-encoded onto ONE line each (entrypoint.sh's `base64 -w0`), so this
// must comfortably exceed the largest findings payload a review can produce;
// 32 MiB is generous headroom over any realistic review.json/usage.json.
const maxPodLogLine = 32 * 1024 * 1024

// parsePodLogs scans r line-by-line, extracting the nonce-scoped
// ===MF-REVIEW-<nonce>-BEGIN/END=== and ===MF-USAGE-<nonce>-BEGIN/END=== marker
// blocks (deploy/sandbox/entrypoint.sh) into outputs["review.json"] /
// outputs["usage.json"] (base64-decoded). This is a PURE function over an
// io.Reader — no clientset access — so tests can feed it a strings.Reader
// directly without a live (or even fake) cluster; the real caller
// (streamPodLogs/rereadPodLogs) feeds it a Pods GetLogs stream.
//
// k8s pod logs merge stdout+stderr with no stream tags, so this is the only
// way to separate the two: any line NOT inside a matching block is opencode's
// narration, written live to stderr (so spec.StreamStderr can stream review
// progress) and kept in the returned stderrTail (capped to stderrTailCap) for
// sandboxStderrTail (M6) diagnostics.
//
// nonce scoping is the v2 C3 anti-forgery guard: manyforge reviews its OWN
// repo, which contains these exact marker strings in source form, so a
// prompt-injected PR could try to print a static/guessed marker to fake a
// result. A block whose nonce doesn't match this run's nonce is therefore
// never recognized as BEGIN/END at all — it's just narration, base64 payload
// included.
//
// A BEGIN with no matching END (the stream was cut off mid-write — log
// rotation, pod eviction, …) is returned as an error so the caller fails the
// lane cleanly instead of silently accepting a zero-cost/empty result.
func parsePodLogs(r io.Reader, nonce string, stderr io.Writer) (outputs map[string][]byte, stderrTail []byte, err error) {
	reviewBegin := fmt.Sprintf("===MF-REVIEW-%s-BEGIN===", nonce)
	reviewEnd := fmt.Sprintf("===MF-REVIEW-%s-END===", nonce)
	usageBegin := fmt.Sprintf("===MF-USAGE-%s-BEGIN===", nonce)
	usageEnd := fmt.Sprintf("===MF-USAGE-%s-END===", nonce)

	outputs = map[string][]byte{}
	var tail bytes.Buffer

	writeNarration := func(line string) {
		if stderr != nil {
			_, _ = io.WriteString(stderr, line)
			_, _ = io.WriteString(stderr, "\n")
		}
		tail.WriteString(line)
		tail.WriteByte('\n')
		if tail.Len() > stderrTailCap {
			b := tail.Bytes()
			kept := append([]byte(nil), b[len(b)-stderrTailCap:]...)
			tail.Reset()
			tail.Write(kept)
		}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxPodLogLine)

	var inBlock bool
	var blockKey, blockEnd string
	var payload bytes.Buffer

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case !inBlock && line == reviewBegin:
			inBlock, blockKey, blockEnd = true, "review.json", reviewEnd
			payload.Reset()
		case !inBlock && line == usageBegin:
			inBlock, blockKey, blockEnd = true, "usage.json", usageEnd
			payload.Reset()
		case inBlock && line == blockEnd:
			if decoded, decErr := base64.StdEncoding.DecodeString(payload.String()); decErr == nil {
				outputs[blockKey] = decoded
			}
			inBlock, blockKey, blockEnd = false, "", ""
		case inBlock:
			payload.WriteString(line)
		default:
			writeNarration(line)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return outputs, tail.Bytes(), fmt.Errorf("kube: reading pod logs: %w", scanErr)
	}
	if inBlock {
		return outputs, tail.Bytes(), fmt.Errorf("kube: truncated %s marker block (BEGIN with no END)", blockKey)
	}
	return outputs, tail.Bytes(), nil
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
// secretKeyRef, never as literal env values. owner (see ownerReferences) ties
// its lifetime to the Job so it can never outlive it, even across a crash.
func (r *KubeRunner) buildSecret(name string, spec sandbox.SandboxSpec, owner []metav1.OwnerReference) *corev1.Secret {
	data := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		data[k] = v
	}
	data[cloneAuthHeaderKey] = spec.CloneAuthHeader
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: runLabels(name), OwnerReferences: owner},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

// buildConfigMap builds the per-run ConfigMap carrying spec.Inputs (the
// review_*.txt files) so the init container can copy them into /out. owner
// (see ownerReferences) ties its lifetime to the Job, same as buildSecret.
func (r *KubeRunner) buildConfigMap(name string, spec sandbox.SandboxSpec, owner []metav1.OwnerReference) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: runLabels(name), OwnerReferences: owner},
		BinaryData: spec.Inputs,
	}
}

// ownerReferences returns a single OwnerReference marking job as the
// controlling owner. Attached to the per-run Secret/ConfigMap so Kubernetes'
// garbage collector deletes them when the Job is deleted — whether that's
// cleanup's best-effort delete on the happy path, or the Job's own
// TTLSecondsAfterFinished/an operator's `kubectl delete job` if this process
// crashes, OOMs, or restarts before cleanup ever runs. Without this, a
// mid-run crash orphans credential-bearing objects (a GitHub PAT, an LLM API
// key) in the cluster permanently — no k8s GC path reaps Secrets/ConfigMaps
// that aren't owned by anything.
func ownerReferences(job *batchv1.Job) []metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := true
	return []metav1.OwnerReference{{
		APIVersion:         "batch/v1",
		Kind:               "Job",
		Name:               job.Name,
		UID:                job.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}}
}

// normalizedTimeout returns spec.Timeout, or a 5 minute default when the
// caller left it unset (<=0). Both Run's context deadline and buildJob's
// activeDeadlineSeconds compute off this single function so a zero Timeout
// can never produce a Job whose activeDeadlineSeconds disagrees with the
// context Run itself waits on.
func normalizedTimeout(spec sandbox.SandboxSpec) time.Duration {
	if spec.Timeout <= 0 {
		return 5 * time.Minute
	}
	return spec.Timeout
}

// buildJob builds the hardened Job for one review run. It is a pure function of
// its inputs (no clientset access) so tests can assert every hardening field
// without a live — or even fake — cluster.
func (r *KubeRunner) buildJob(name string, spec sandbox.SandboxSpec, markerNonce string) *batchv1.Job {
	backoffLimit := int32(0)
	ttl := jobTTLSecondsAfterFinished
	activeDeadline := int64(normalizedTimeout(spec).Seconds()) + cloneMarginSeconds

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

	// The real API server rejects an imagePullSecrets entry with an empty name,
	// so only emit one when PullSecret is actually configured.
	var imagePullSecrets []corev1.LocalObjectReference
	if r.PullSecret != "" {
		imagePullSecrets = []corev1.LocalObjectReference{{Name: r.PullSecret}}
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
					ImagePullSecrets:             imagePullSecrets,
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
