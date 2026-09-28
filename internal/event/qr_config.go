package event

import (
	"encoding/json"
	"fmt"
	"regexp"
)

const maxQRConfigBytes = 400_000

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateQRConfig(config []byte) error {
	if len(config) > maxQRConfigBytes {
		return fmt.Errorf("QR configuration is too large")
	}

	var value map[string]json.RawMessage
	if err := json.Unmarshal(config, &value); err != nil {
		return fmt.Errorf("invalid QR configuration: %w", err)
	}
	if value == nil {
		return fmt.Errorf("QR configuration must be an object")
	}

	for _, field := range []string{"fgColor", "bgColor", "cardBgColor", "frameColor"} {
		if err := validateQRColor(value, field); err != nil {
			return err
		}
	}
	return nil
}

func validateQRColor(config map[string]json.RawMessage, field string) error {
	raw, exists := config[field]
	if !exists || string(raw) == "null" {
		return nil
	}

	var color string
	if err := json.Unmarshal(raw, &color); err != nil || !hexColorPattern.MatchString(color) {
		return fmt.Errorf("QR configuration %s must be a hex color", field)
	}
	return nil
}
