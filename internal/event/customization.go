package event

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
)

// FeatureGate is the plan check the event service needs. A nil gate allows
// everything.
type FeatureGate interface {
	Require(ctx context.Context, eventID uuid.UUID, feature catalog.Feature) error
}

// Basic customization is what every plan may save: pick one of the guest page
// presets and write the page's words, and pick one of the QR colour presets.
// Anything beyond that — custom colours, fonts, cover images, QR dot and
// corner styles, a QR logo — needs customization.full.
//
// The preset tables mirror the frontend's GUEST_THEME_PRESETS and
// QR_COLOR_PRESETS. When a preset is added there, add it here too; until then
// choosing it is treated as full customization.

type guestThemePreset struct {
	PrimaryColor, BgColor, SurfaceColor string
	FontHeading, FontBody               string
	HeroStyle, GalleryLayout            string
	CameraFrame, SampleCoverURL         string
}

const unsplash = "https://images.unsplash.com/photo-"

var guestThemePresets = map[string]guestThemePreset{
	"editorial": {"#181e17", "#fdfbf7", "#ffffff", "serif", "sans", "banner", "masonry", "minimal", unsplash + "1519741497674-611481863552?auto=format&fit=crop&w=1200&q=80"},
	"minimal":   {"#0f172a", "#ffffff", "#f8fafc", "sans", "sans", "monogram", "grid", "minimal", unsplash + "1511285560929-80b456fea0bc?auto=format&fit=crop&w=1200&q=80"},
	"romantic":  {"#881337", "#fff1f2", "#ffffff", "serif", "serif", "avatar", "masonry", "polaroid", unsplash + "1583939003579-730e3918a45a?auto=format&fit=crop&w=1200&q=80"},
	"botanical": {"#2d4a3e", "#f4f6f0", "#ffffff", "serif", "sans", "banner", "masonry", "minimal", unsplash + "1465495976277-4387d4b0b4c6?auto=format&fit=crop&w=1200&q=80"},
	"film":      {"#c2410c", "#faf6ee", "#fffdf8", "mono", "sans", "banner", "masonry", "35mm", unsplash + "1532712938310-34cb3982ef74?auto=format&fit=crop&w=1200&q=80"},
	"vintage":   {"#3f2b1d", "#f7f4ea", "#ffffff", "classic", "classic", "monogram", "masonry", "polaroid", unsplash + "1520854221256-17451cc331bf?auto=format&fit=crop&w=1200&q=80"},
	"modern":    {"#312e81", "#f8fafc", "#ffffff", "sans", "sans", "banner", "grid", "minimal", unsplash + "1492684223066-81342ee5ff30?auto=format&fit=crop&w=1200&q=80"},
	"luxury":    {"#d4af37", "#09090b", "#18181b", "serif", "sans", "avatar", "masonry", "gold", unsplash + "1511795409834-ef04bbd61622?auto=format&fit=crop&w=1200&q=80"},
}

type qrColorPreset struct{ Fg, Bg, CardBg string }

var qrColorPresets = []qrColorPreset{
	{"#181e17", "#ffffff", "#ffffff"}, // classic
	{"#46533a", "#fffefa", "#f8f7f2"}, // moss
	{"#1e3a5f", "#ffffff", "#f8fafc"}, // navy
	{"#1a472a", "#f0faf0", "#f5faf5"}, // forest
	{"#722f37", "#fff8f0", "#fdf6f0"}, // wine
	{"#e8e8e8", "#1a1a2e", "#1a1a2e"}, // midnight
	{"#0077b6", "#f0f8ff", "#f0f8ff"}, // ocean
	{"#c1440e", "#fff5eb", "#fff5eb"}, // sunset
}

// Default QR styling. A basic QR keeps these; changing them is full styling.
var qrBasicStyle = map[string]string{
	"dotType":          "rounded",
	"cornerSquareType": "extra-rounded",
	"cornerDotType":    "dot",
	"frameColor":       "#dcded2",
}

// isBasicGuestTheme reports whether theme only uses preset styling. Content
// fields (titles, welcome text, CTA, prompts, monogram) are always allowed.
func isBasicGuestTheme(theme []byte) bool {
	var config map[string]any
	if err := json.Unmarshal(theme, &config); err != nil || config == nil {
		// Malformed themes are not this check's concern; nothing to charge for.
		return true
	}
	presetID, _ := config["presetId"].(string)
	preset, known := guestThemePresets[presetID]
	if !known {
		// Without a preset there is nothing styling could be "basic" against;
		// only an empty styling block is basic.
		for _, field := range []string{"primaryColor", "bgColor", "surfaceColor", "fontHeading", "fontBody", "heroStyle", "galleryLayout", "cameraFrame", "coverUrl"} {
			if present(config[field]) {
				return false
			}
		}
		return true
	}
	expected := map[string]string{
		"primaryColor":  preset.PrimaryColor,
		"bgColor":       preset.BgColor,
		"surfaceColor":  preset.SurfaceColor,
		"fontHeading":   preset.FontHeading,
		"fontBody":      preset.FontBody,
		"heroStyle":     preset.HeroStyle,
		"galleryLayout": preset.GalleryLayout,
		"cameraFrame":   preset.CameraFrame,
	}
	for field, want := range expected {
		if !matchesOrAbsent(config[field], want) {
			return false
		}
	}
	return matchesOrAbsent(config["coverUrl"], preset.SampleCoverURL)
}

// isBasicQRConfig reports whether config only uses a colour preset and the
// default styling, with no logo.
func isBasicQRConfig(qr []byte) bool {
	var config map[string]any
	if err := json.Unmarshal(qr, &config); err != nil || config == nil {
		return true
	}
	if present(config["logoUrl"]) {
		return false
	}
	for field, want := range qrBasicStyle {
		if !matchesOrAbsent(config[field], want) {
			return false
		}
	}
	fg, bg, card := config["fgColor"], config["bgColor"], config["cardBgColor"]
	if !present(fg) && !present(bg) && !present(card) {
		return true
	}
	for _, p := range qrColorPresets {
		if matchesOrAbsent(fg, p.Fg) && matchesOrAbsent(bg, p.Bg) && matchesOrAbsent(card, p.CardBg) {
			return true
		}
	}
	return false
}

func present(v any) bool {
	switch value := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	default:
		return true
	}
}

func matchesOrAbsent(v any, want string) bool {
	if !present(v) {
		return true
	}
	s, ok := v.(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), want)
}
