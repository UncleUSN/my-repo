package paths

import "testing"

func TestCleanWindows(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:\ferry\inbox\a.txt`, `C:\ferry\inbox\a.txt`},
		{`c:/ferry/inbox/a.txt`, `C:\ferry\inbox\a.txt`},
		{`C:\ferry\..\ferry\inbox\`, `C:\ferry\inbox`},
		{`C:\`, `C:\`},
		{`C:/`, `C:\`},
		{`\\server\share\a\..\b`, `\\server\share\b`},
		{`\\?\C:\ferry\inbox`, `C:\ferry\inbox`},
		{`\\?\UNC\server\share\a`, `\\server\share\a`},
		{`inbox\a.txt`, `inbox\a.txt`},
	}
	for _, tc := range cases {
		got, err := Clean(tc.in)
		if err != nil {
			t.Fatalf("Clean(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("Clean(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanRejectsEscapeAndJunk(t *testing.T) {
	bad := []string{
		`C:\ferry\..\..\Windows`,
		`C:relative`,
		"",
		"foo\x00bar",
		"/tmp/a\nb",
	}
	for _, in := range bad {
		if _, err := Clean(in); err == nil {
			t.Fatalf("Clean(%q) succeeded, want error", in)
		}
	}
}

func TestCleanPOSIX(t *testing.T) {
	got, err := Clean("/home/dmitry/../dmitry/ferry/inbox/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/dmitry/ferry/inbox" {
		t.Fatalf("got %q", got)
	}
	got, err = Clean("/mnt/c/Users/Dmitry")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/c/Users/Dmitry" {
		t.Fatalf("WSL mount path got %q", got)
	}
}

func TestWithinRoot(t *testing.T) {
	okCases := []struct{ path, root string }{
		{`C:\ferry\inbox\a.txt`, `C:\ferry`},
		{`c:\FERRY\Inbox\a.txt`, `C:\ferry`},
		{`C:\ferry`, `C:\ferry`},
		{`C:\ferry\`, `C:\ferry`},
		{`/home/dmitry/ferry/inbox/a`, `/home/dmitry/ferry`},
		{`/home/dmitry/ferry`, `/home/dmitry/ferry`},
		{`/`, `/`},
		{`/tmp/a`, `/`},
	}
	for _, tc := range okCases {
		ok, err := WithinRoot(tc.path, tc.root)
		if err != nil || !ok {
			t.Fatalf("WithinRoot(%q, %q)=%v,%v want true", tc.path, tc.root, ok, err)
		}
	}
	bad := []struct{ path, root string }{
		{`C:\ferry2\a`, `C:\ferry`},
		{`C:\ferry\..\Windows\a`, `C:\ferry`},
		{`D:\ferry\a`, `C:\ferry`},
		{`/home/dmitry/ferry2/a`, `/home/dmitry/ferry`},
		{`/etc/passwd`, `/home/dmitry`},
		{`C:\ferry\a`, `/home/dmitry`},
		{`/home/dmitry/a`, `C:\ferry`},
	}
	for _, tc := range bad {
		ok, err := WithinRoot(tc.path, tc.root)
		if err != nil {
			// Побег за корень диска — тоже отказ, не «внутри».
			continue
		}
		if ok {
			t.Fatalf("WithinRoot(%q, %q) = true, want false", tc.path, tc.root)
		}
	}
}

func TestJoin(t *testing.T) {
	cases := []struct {
		dir, rel, want string
	}{
		{`C:\ferry\inbox`, `a.txt`, `C:\ferry\inbox\a.txt`},
		{`C:/ferry/inbox/`, `sub/a.txt`, `C:\ferry\inbox\sub\a.txt`},
		{`C:\`, `a.txt`, `C:\a.txt`},
		{`/home/dmitry/ferry/inbox`, `a.txt`, `/home/dmitry/ferry/inbox/a.txt`},
		{`/home/dmitry/ferry/inbox/`, `sub/a.txt`, `/home/dmitry/ferry/inbox/sub/a.txt`},
		{`/`, `a.txt`, `/a.txt`},
	}
	for _, tc := range cases {
		got, err := Join(tc.dir, tc.rel)
		if err != nil {
			t.Fatalf("Join(%q, %q): %v", tc.dir, tc.rel, err)
		}
		if got != tc.want {
			t.Fatalf("Join(%q, %q)=%q, want %q", tc.dir, tc.rel, got, tc.want)
		}
	}
	if _, err := Join(`/home/a`, `../etc/passwd`); err == nil {
		t.Fatal("Join allowed ..")
	}
	if _, err := Join(`/home/a`, `/etc/passwd`); err == nil {
		t.Fatal("Join allowed absolute rel")
	}
}

func TestMatch(t *testing.T) {
	ok, err := Match("*.log", `sub/app.log`)
	if err != nil || !ok {
		t.Fatalf("base match: %v %v", ok, err)
	}
	ok, err = Match("*.log", `sub/app.txt`)
	if err != nil || ok {
		t.Fatalf("unexpected match: %v %v", ok, err)
	}
	ok, err = Match("sub/*.log", `sub/app.log`)
	if err != nil || !ok {
		t.Fatalf("rel match: %v %v", ok, err)
	}
	// В шаблоне нет «/», поэтому сравнивается базовое имя: «*» совпадает с «b».
	ok, err = Match("*", `a/b`)
	if err != nil || !ok {
		t.Fatalf("* against base of a/b: %v %v", ok, err)
	}
	ok, err = Match("a/*", `a/b/c`)
	if err != nil || ok {
		t.Fatal("star must not cross a slash when the pattern itself contains a slash")
	}
}

func TestIsAbs(t *testing.T) {
	if !IsAbs(`C:\ferry`) || !IsAbs(`C:/ferry`) || !IsAbs(`\\server\share`) {
		t.Fatal("windows abs")
	}
	if IsAbs(`inbox\a`) || IsAbs(`C:foo`) {
		t.Fatal("not abs")
	}
	if !IsAbs(`/home/a`) || IsAbs(`home/a`) {
		t.Fatal("posix abs")
	}
}
