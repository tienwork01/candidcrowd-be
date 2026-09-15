package media

import "testing"

func TestUploadValidation(t *testing.T) {
	tests := []struct {
		mime      string
		allowed   bool
		extension string
	}{{"image/jpeg", true, ".jpg"}, {"video/mp4", true, ".mp4"}, {"application/pdf", false, ""}}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := allowed(tt.mime); got != tt.allowed {
				t.Fatalf("allowed(%q)=%v", tt.mime, got)
			}
			if got := extension(tt.mime); got != tt.extension {
				t.Fatalf("extension(%q)=%q", tt.mime, got)
			}
		})
	}
}
