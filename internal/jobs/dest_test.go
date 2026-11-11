package jobs

import (
	"testing"

	"github.com/UncleUSN/my-repo/internal/config"
)

func TestDestinationPath(t *testing.T) {
	cases := []struct {
		name   string
		job    config.Job
		srcDir bool
		rel    string
		want   string
	}{
		{
			name:   "windows dir from posix host",
			job:    config.Job{Destination: config.Destination{Path: `C:\ferry\inbox`}},
			srcDir: true,
			rel:    "a.txt",
			want:   `C:\ferry\inbox\a.txt`,
		},
		{
			name:   "windows slashes and nested",
			job:    config.Job{Destination: config.Destination{Path: `C:/ferry/inbox/`}},
			srcDir: false,
			rel:    "sub/a.txt",
			want:   `C:\ferry\inbox\sub\a.txt`,
		},
		{
			name: "exact file",
			job:  config.Job{Destination: config.Destination{Path: `C:\ferry\inbox\a.txt`}},
			rel:  "a.txt",
			want: `C:\ferry\inbox\a.txt`,
		},
		{
			name: "wsl dest",
			job:  config.Job{Watch: `C:\ferry\outbox`, Destination: config.Destination{Path: "/home/dmitry/ferry/inbox"}},
			rel:  "sub/a.txt",
			want: "/home/dmitry/ferry/inbox/sub/a.txt",
		},
		{
			name:   "posix root",
			job:    config.Job{Destination: config.Destination{Path: "/"}},
			srcDir: true,
			rel:    "a.txt",
			want:   "/a.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := destinationPath(tc.job, tc.srcDir, tc.rel)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if _, err := destinationPath(config.Job{Destination: config.Destination{Path: "/data"}}, true, "../etc/passwd"); err == nil {
		t.Fatal("relative escape was accepted")
	}
}
