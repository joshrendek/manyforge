// Package clidist serves the prebuilt `mf` Work Board CLI binaries that the
// container image ships under /cli (see Dockerfile), plus a small POSIX install
// script, so a repo or sandbox can fetch the CLI straight from its ManyForge
// instance:
//
//	curl -fsSL https://hub.example.com/cli/install.sh | sh
//
// Only a fixed allowlist of os/arch pairs is served; the request never names a
// file path, so there is nothing to traverse.
package clidist

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// platforms is the closed set of binaries the image builds (Dockerfile stage 2).
var platforms = map[string]bool{
	"darwin/amd64": true,
	"darwin/arm64": true,
	"linux/amd64":  true,
	"linux/arm64":  true,
}

// Handler serves GET /cli/install.sh and GET /cli/mf/{os}/{arch}.
type Handler struct {
	// Dir holds mf-<os>-<arch> binaries. A missing file is a plain 404, so dev
	// builds without /cli simply don't offer downloads.
	Dir string
	// BaseURL is the externally reachable instance origin baked into the
	// install script. Empty falls back to the request's own scheme + host.
	BaseURL string
}

// Routes mounts the download routes on r.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/cli/install.sh", h.installScript)
	r.Get("/cli/mf/{os}/{arch}", h.binary)
}

func (h *Handler) binary(w http.ResponseWriter, r *http.Request) {
	goos, goarch := chi.URLParam(r, "os"), chi.URLParam(r, "arch")
	if !platforms[goos+"/"+goarch] {
		http.NotFound(w, r)
		return
	}
	name := "mf-" + goos + "-" + goarch
	f, err := os.Open(filepath.Join(h.Dir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="mf"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (h *Handler) installScript(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(h.BaseURL, "/")
	if base == "" {
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		base = scheme + "://" + r.Host
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, InstallScript(base))
}

// InstallScript renders the installer for an instance at base.
func InstallScript(base string) string {
	return strings.ReplaceAll(installTemplate, "@@BASE@@", shellQuote(base))
}

// shellQuote single-quotes s for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

const installTemplate = `#!/bin/sh
# Installs the ManyForge Work Board CLI (mf).
set -eu
base=@@BASE@@
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "mf: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "mf: unsupported OS: $os" >&2; exit 1 ;;
esac
dir=${MF_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$dir"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$base/cli/mf/$os/$arch" -o "$tmp"
chmod 0755 "$tmp"
mv -f "$tmp" "$dir/mf"
trap - EXIT
echo "mf installed to $dir/mf"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: add $dir to your PATH" ;;
esac
echo "next: cd <repo> && mf init --server $base --project <slug> && mf login --token <mfw_...> && mf setup claude"
`
