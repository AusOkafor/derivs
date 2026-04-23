package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// EmailToPlaceholderTelegramUsername builds a telegram_username-shaped key (a–z, 0–9, _)
// from the buyer email when checkout had no Telegram handle yet. Used for Supabase rows
// until the user links their real @username via /api/subscribe.
func EmailToPlaceholderTelegramUsername(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	var b strings.Builder
	for _, r := range email {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@':
			b.WriteString("_at_")
		case r == '.', r == '+', r == '-':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if len(s) > 32 {
		h := sha256.Sum256([]byte(email))
		short := hex.EncodeToString(h[:])[:8]
		prefix := s
		if len(prefix) > 23 {
			prefix = prefix[:23]
		}
		s = prefix + "_" + short
		if len(s) > 32 {
			s = s[:32]
		}
	}
	if s == "" {
		s = "billing_user"
	}
	return s
}
