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

func TestEnsureNtfyMarkdownFormat(t *testing.T) {
	// apprise-go's ntfy handler only honours ?format=markdown (it sets the
	// X-Markdown header from it); ?markdown=yes is ignored and would arrive as
	// plain text.
	got := ensureNtfyMarkdownFormat("ntfy://backup.example.com/alerts?priority=high")
	want := "ntfy://backup.example.com/alerts?format=markdown&priority=high"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnsureNtfyMarkdownFormatIdempotent(t *testing.T) {
	in := "ntfy://ntfy.sh/backup?format=markdown"
	if got := ensureNtfyMarkdownFormat(in); got != in {
		t.Errorf("should keep existing format=markdown, got %q", got)
	}
}

func TestEnsureNtfyMarkdownFormatKeepsLegacyMarkdown(t *testing.T) {
	// legacy ?markdown=yes is harmless; ensureNtfyMarkdownFormat adds the
	// format=markdown that apprise-go actually needs.
	got := ensureNtfyMarkdownFormat("ntfys://ntfy.yufei.im/Backup?markdown=yes")
	want := "ntfys://ntfy.yufei.im/Backup?format=markdown&markdown=yes"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnsureNtfyMarkdownFormatInvalid(t *testing.T) {
	if got := ensureNtfyMarkdownFormat("://bad url"); got != "://bad url" {
		t.Errorf("invalid url should pass through, got %q", got)
	}
}
