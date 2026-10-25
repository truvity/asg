package asg

import (
	"encoding/base64"
	"unicode/utf8"
)

func utf8Valid(b []byte) bool { return utf8.Valid(b) }

func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
