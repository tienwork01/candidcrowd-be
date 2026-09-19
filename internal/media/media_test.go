package media

import "testing"

func TestUploadValidation(t *testing.T) {
	tests := []struct {
		mime      string
		allowed   bool
		extension string
	}{
		{"image/jpeg", true, ".jpg"},
		{"image/png", true, ".png"},
		{"image/webp", true, ".webp"},
		{"video/mp4", true, ".mp4"},
		{"video/quicktime", true, ".mov"},
		{"image/svg+xml", false, ""},
		{"image/gif", false, ""},
		{"application/pdf", false, ""},
		{"text/html", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := allowed(tt.mime); got != tt.allowed {
				t.Fatalf("allowed(%q)=%v, want %v", tt.mime, got, tt.allowed)
			}
			if got := extension(tt.mime); got != tt.extension {
				t.Fatalf("extension(%q)=%q, want %q", tt.mime, got, tt.extension)
			}
		})
	}
}
