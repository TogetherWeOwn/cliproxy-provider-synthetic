package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target is linux/amd64")
	}
	out := t.TempDir()
	cmd := exec.Command("bash", "scripts/package-release.sh", out)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, result)
	}
	// The archive name tracks registry.json, the same source the packaging
	// script reads, so version bumps cannot desynchronize the two.
	var registry struct {
		Plugins []struct {
			Version string `json:"version"`
		} `json:"plugins"`
	}
	rawRegistry, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawRegistry, &registry); err != nil || len(registry.Plugins) != 1 {
		t.Fatalf("invalid registry.json: %v", err)
	}
	name := "synthetic_" + registry.Plugins[0].Version + "_linux_amd64.zip"
	archive := filepath.Join(out, name)
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	checksums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), hex.EncodeToString(sum[:])+"  "+name) {
		t.Fatalf("incorrect checksums %s", checksums)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	found := false
	for _, f := range reader.File {
		if f.Name == "synthetic.so" && f.UncompressedSize64 > 0 {
			found = true
		}
		if strings.Contains(f.Name, "/") || strings.HasSuffix(f.Name, ".h") {
			t.Fatalf("unexpected archive entry %q", f.Name)
		}
	}
	if !found {
		t.Fatal("archive lacks synthetic.so")
	}
}
