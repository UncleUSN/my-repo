//go:build !windows

package paths

import "testing"

func TestForLocalOSUnix(t *testing.T) {
	got, err := ForLocalOS("/home/dmitry/../dmitry/ferry")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/dmitry/ferry" {
		t.Fatalf("got %q", got)
	}
	if _, err := ForLocalOS(`C:\ferry\inbox\a.txt`); err == nil {
		t.Fatal("Windows path was accepted on unix")
	}
	if _, err := ForLocalOS(`C:/ferry/inbox/a.txt`); err == nil {
		t.Fatal("Windows path with slashes was accepted on unix")
	}
}
