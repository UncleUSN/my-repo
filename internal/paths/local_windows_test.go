//go:build windows

package paths

import "testing"

func TestForLocalOSWindows(t *testing.T) {
	got, err := ForLocalOS(`C:/ferry/inbox/a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `C:\ferry\inbox\a.txt` {
		t.Fatalf("got %q", got)
	}
	got, err = ForLocalOS(`inbox/a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `inbox\a.txt` {
		t.Fatalf("relative got %q", got)
	}
	if _, err := ForLocalOS(`/home/dmitry/ferry`); err == nil {
		t.Fatal("POSIX absolute path was accepted on Windows")
	}
}
