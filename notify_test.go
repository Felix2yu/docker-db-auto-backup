package main

import "testing"

func TestIsNtfyURL(t *testing.T) {
	cases := map[string]bool{
		"ntfy://ntfy.sh/backup":   true,
		"NTFY://ntfy.sh/backup":   true,
		"slack://token/token/tok": false,
		"ntfy.sh/backup":          false,
	}
	for raw, want := range cases {
		if got := isNtfyURL(raw); got != want {
			t.Errorf("isNtfyURL(%q): got %v, want %v", raw, got, want)
		}
	}
}

func TestEnableNtfyMarkdown(t *testing.T) {
	got := enableNtfyMarkdown("ntfy://backup.example.com/alerts?priority=high")
	want := "ntfy://backup.example.com/alerts?markdown=yes&priority=high"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnableNtfyMarkdownIdempotent(t *testing.T) {
	got := enableNtfyMarkdown("ntfy://ntfy.sh/backup?markdown=yes")
	if got != "ntfy://ntfy.sh/backup?markdown=yes" {
		t.Errorf("should not duplicate markdown param, got %q", got)
	}
}

func TestEnableNtfyMarkdownInvalid(t *testing.T) {
	if got := enableNtfyMarkdown("://bad url"); got != "://bad url" {
		t.Errorf("invalid url should pass through, got %q", got)
	}
}
