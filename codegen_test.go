package kalathos

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGeneratedClientUpToDate(t *testing.T) {
	if os.Getenv("SKIP_CODEGEN_CHECK") == "1" {
		t.Skip("SKIP_CODEGEN_CHECK=1")
	}
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	cmd := exec.Command("go", "generate", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go generate: %v\n%s", err, out)
	}
	diff := exec.Command("git", "diff", "--quiet", "internal/apiclient/client.gen.go", "openapi.codegen.json")
	diff.Dir = dir
	if err := diff.Run(); err != nil {
		t.Fatal("generated code is stale; run go generate in packages/sdk-go")
	}
}
