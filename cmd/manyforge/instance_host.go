package main

import (
	"fmt"

	"github.com/manyforge/manyforge/internal/portaldomain"
)

// resolveInstanceHost derives the instance hostname used by portal host routing
// from the public base URL. An empty URL means host routing is off; a non-empty
// URL that cannot yield a usable host is an error (startup must fail rather than
// silently turn routing off and expose the operator app on extra hostnames). The
// error never echoes the URL, which may carry userinfo.
func resolveInstanceHost(baseURL string) (string, error) {
	h, err := portaldomain.InstanceHost(baseURL)
	if err != nil {
		return "", fmt.Errorf("public base URL rejected: %w", err)
	}
	return h, nil
}
