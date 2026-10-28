package wakeproxy

import (
	"encoding/json"
	"fmt"

	"github.com/truvity/asg/internal/wakeconfig"
)

func jsonIndent(c wakeconfig.Config) (string, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}

	return string(b) + "\n", nil
}
