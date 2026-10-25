package asg

import (
	"encoding/base64"
	"unicode/utf8"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func utf8Valid(b []byte) bool { return utf8.Valid(b) }

func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func pulumiString(s string) pulumi.StringInput { return pulumi.String(s) }
