// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
// modulezip stages the exact Go module archive and h1 digest verified after publication.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/zip"
)

func run() error {
	directory := flag.String("dir", "", "module source directory")
	version := flag.String("version", "", "canonical mapped Go module version, including v")
	output := flag.String("output", "", "destination module zip")
	flag.Parse()
	if *directory == "" || *version == "" || *output == "" {
		return fmt.Errorf("dir, version and output are required")
	}
	identity := module.Version{Path: "github.com/joshrendek/manyforge/sdk/go", Version: *version}
	if err := module.Check(identity.Path, identity.Version); err != nil {
		return err
	}
	archive, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := zip.CreateFromDir(archive, identity, *directory); err != nil {
		_ = archive.Close()
		_ = os.Remove(*output)
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	digest, err := dirhash.HashZip(*output, dirhash.Hash1)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Path    string `json:"path"`
		Version string `json:"version"`
		Sum     string `json:"sum"`
	}{identity.Path, identity.Version, digest})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
