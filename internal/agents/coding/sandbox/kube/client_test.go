package kube

import "testing"

// TestInClusterClientsetOffCluster pins Task 4.1: off-cluster (no SA token/env),
// InClusterConfig fails, and InClusterClientset must surface that error rather than
// panic. This test runs in the normal `go test` environment (never in-cluster).
func TestInClusterClientsetOffCluster(t *testing.T) {
	cs, err := InClusterClientset()
	if err == nil {
		t.Fatal("InClusterClientset: want error when running off-cluster, got nil")
	}
	if cs != nil {
		t.Fatalf("InClusterClientset: want nil clientset on error, got %+v", cs)
	}
}

// TestNamespaceDefaultsWhenSAFileAbsent pins Task 4.1: when the in-cluster
// service-account namespace file isn't present (any dev/test/CI box), Namespace()
// falls back to "manyforge-sandbox" rather than erroring.
func TestNamespaceDefaultsWhenSAFileAbsent(t *testing.T) {
	got := Namespace()
	if got != "manyforge-sandbox" {
		t.Fatalf("Namespace() = %q, want %q", got, "manyforge-sandbox")
	}
}
