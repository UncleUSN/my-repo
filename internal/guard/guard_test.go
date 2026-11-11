package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInsideRoot(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.Mkdir(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Resolve(filepath.Join(inbox, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(inbox, "a.txt")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if _, err := g.Resolve(outside); err == nil {
		t.Fatal("path outside root was accepted")
	}
}

func TestSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Resolve(filepath.Join(link, "secret")); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func TestOpenGuard(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Open() {
		t.Fatal("expected open guard")
	}
	dir := t.TempDir()
	got, err := g.Resolve(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "a") {
		t.Fatalf("got %s", got)
	}
}
