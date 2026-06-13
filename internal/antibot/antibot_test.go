package antibot

import "testing"

func TestDetectBlocked(t *testing.T) {
	blocked := []string{
		"<title>Just a moment...</title>",
		"You've been blocked by network security",
		"we need you to prove you're a human before continuing",
		"Pardon Our Interruption",
		`<div class="g-recaptcha" data-sitekey="x"></div>`,
		"Our systems have detected unusual traffic from your network",
	}
	for _, b := range blocked {
		if marker, ok := Detect(b); !ok {
			t.Errorf("Detect(%q) = false, want true", b)
		} else if marker == "" {
			t.Errorf("Detect(%q) returned empty marker", b)
		}
	}
}

func TestDetectClean(t *testing.T) {
	clean := "# Kubernetes monitoring\n\nVictoriaMetrics scrapes targets every 30s. " +
		"This is ordinary article content with no challenge text whatsoever."
	if marker, ok := Detect(clean); ok {
		t.Errorf("Detect(clean) matched %q, want no match", marker)
	}
}
