package checksum

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, n, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("size %d", n)
	}
	// echo -n hello | sha256sum
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if !Equal(sum, want) {
		t.Fatalf("sum %s", sum)
	}
	if !Equal(want, "2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824") {
		t.Fatal("case")
	}
}

func TestEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, n, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal(n)
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if sum != empty {
		t.Fatalf("sum %s", sum)
	}
	if !ValidSHA256(empty) || ValidSHA256("abc") || ValidSHA256("") {
		t.Fatal("ValidSHA256")
	}
}
