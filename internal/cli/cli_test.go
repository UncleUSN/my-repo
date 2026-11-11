package cli

import "testing"

func TestHelpAndVersion(t *testing.T) {
	if code := Main("0.1.0-test", nil); code != 2 {
		t.Fatalf("empty args: %d", code)
	}
	if code := Main("0.1.0-test", []string{"help"}); code != 0 {
		t.Fatalf("help: %d", code)
	}
	if code := Main("0.1.0-test", []string{"version"}); code != 0 {
		t.Fatalf("version: %d", code)
	}
	if code := Main("0.1.0-test", []string{"nope"}); code != 2 {
		t.Fatalf("unknown: %d", code)
	}
	if code := Main("0.1.0-test", []string{"send"}); code != 2 {
		t.Fatalf("send without flags: %d", code)
	}
}
