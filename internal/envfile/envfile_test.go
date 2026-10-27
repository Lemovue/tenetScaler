package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n\nTS_ENVFILE_A=alpha\nexport TS_ENVFILE_B = \"beta\"\nTS_ENVFILE_C='gamma'\nTS_ENVFILE_SET=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TS_ENVFILE_SET", "from-env")
	for _, k := range []string{"TS_ENVFILE_A", "TS_ENVFILE_B", "TS_ENVFILE_C"} {
		t.Setenv(k, "") // registers cleanup; Unset below makes it truly unset
		os.Unsetenv(k)
	}
	if err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{"TS_ENVFILE_A": "alpha", "TS_ENVFILE_B": "beta", "TS_ENVFILE_C": "gamma", "TS_ENVFILE_SET": "from-env"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadSkipsByteOrderMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("\ufeff# written by PowerShell\nTS_ENVFILE_BOM=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TS_ENVFILE_BOM", "")
	os.Unsetenv("TS_ENVFILE_BOM")
	if err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("TS_ENVFILE_BOM"); got != "yes" {
		t.Fatalf("TS_ENVFILE_BOM = %q, want yes", got)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}

func TestLoadRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("NOT_A_PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err == nil {
		t.Fatal("expected an error for a line without '='")
	}
}

func TestGet(t *testing.T) {
	t.Setenv("TS_ENVFILE_GET", "  value ")
	if got := Get("TS_ENVFILE_GET", "def"); got != "value" {
		t.Errorf("Get = %q, want trimmed value", got)
	}
	t.Setenv("TS_ENVFILE_GET", "   ")
	if got := Get("TS_ENVFILE_GET", "def"); got != "def" {
		t.Errorf("blank value should fall back to the default, got %q", got)
	}
}
