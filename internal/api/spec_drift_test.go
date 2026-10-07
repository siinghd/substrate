//go:build spec

// Spec-drift guard: every route registered by the API server must exist in
// docs/openapi.yaml and vice versa. Run via `go test -tags spec ./internal/api`.
package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	pkgDir, _ := os.Getwd()
	repoRoot := pkgDir
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			break
		}
		repoRoot = filepath.Dir(repoRoot)
	}

	specBytes, err := os.ReadFile(filepath.Join(repoRoot, "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(specBytes, &spec); err != nil {
		t.Fatal(err)
	}
	specRoutes := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch strings.ToLower(method) {
			case "get", "post", "put", "delete", "patch":
				specRoutes[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	srcBytes, err := os.ReadFile(filepath.Join(pkgDir, "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"(GET|POST|PUT|DELETE|PATCH) (/v1/[^"]*)"`)
	implRoutes := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(srcBytes), -1) {
		p := strings.Replace(m[2], "{path...}", "{path}", 1)
		implRoutes[m[1]+" "+p] = true
	}

	var missing []string
	for r := range implRoutes {
		if !specRoutes[r] {
			missing = append(missing, "impl-not-in-spec: "+r)
		}
	}
	var undocumented []string
	for r := range specRoutes {
		// normalize {var}/{id} shapes the mux registers vs spec wildcards
		if !specRouteCovers(specRoutes, r) {
			undocumented = append(undocumented, "spec-route-never-registered: "+r)
		}
	}
	sort.Strings(missing)
	sort.Strings(undocumented)
	for _, m := range missing {
		t.Error(m)
	}
	for _, u := range undocumented {
		t.Error(u)
	}
}

func specRouteCovers(spec map[string]bool, impl string) bool {
	if spec[impl] {
		return true
	}
	return false
}
