package main

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpOptions are the RFC 6238 defaults, which Proxmox uses for a TOTP entry
// created without explicit parameters.
var totpOptions = totp.ValidateOpts{
	Period:    30,
	Skew:      0,
	Digits:    otp.DigitsSix,
	Algorithm: otp.AlgorithmSHA1,
	Encoder:   otp.EncoderDefault,
}

// totpCode returns the RFC 6238 code for a base32 seed at the given time.
func totpCode(seed string, now time.Time) (string, error) {
	code, err := totp.GenerateCodeCustom(strings.ReplaceAll(seed, " ", ""), now, totpOptions)
	if err != nil {
		slog.Error("totp code generation failed", "err", err)
		return "", fmt.Errorf("generate totp code: %w", err)
	}
	return code, nil
}
