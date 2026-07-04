// Package kube provides the in-cluster Kubernetes client seam for the sandbox
// KubeRunner (Phase 4, Task 4.5). This task only wires up the client + namespace
// lookup; the runner itself lands separately.
package kube

import (
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// defaultNamespace is used whenever the in-cluster service-account namespace file
// can't be read (off-cluster, or a misconfigured pod).
const defaultNamespace = "manyforge-sandbox"

// serviceAccountNamespaceFile is the well-known path Kubernetes projects the pod's
// namespace into every container via the default service-account token mount.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// InClusterClientset builds a Kubernetes clientset from the in-cluster
// configuration (the service-account token/CA/host Kubernetes injects into every
// pod). Off-cluster — e.g. local dev, CI, unit tests — rest.InClusterConfig
// returns an error because that environment isn't present; InClusterClientset
// propagates that error to the caller rather than panicking, so callers can decide
// whether to fall back to another sandbox mode.
func InClusterClientset() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// Namespace returns the namespace the running pod belongs to, read from the
// service-account namespace file Kubernetes mounts into every container. On any
// error (file absent, unreadable — i.e. off-cluster), it defaults to
// "manyforge-sandbox" rather than failing.
func Namespace() string {
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return defaultNamespace
	}
	ns := string(b)
	if ns == "" {
		return defaultNamespace
	}
	return ns
}
