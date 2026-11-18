package domain

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Rules are the configurable limits applied when creating notifications.
type Rules struct {
	// MaxContentLength is counted in characters (runes), per channel.
	MaxContentLength        map[Channel]int
	MaxBatchSize            int
	MaxScheduleAhead        time.Duration
	MaxAttempts             int
	MaxIdempotencyKeyLength int
}

// DefaultRules returns the limits used when no configuration overrides them.
//
// SMS is capped at 160 characters, i.e. one GSM-7 segment; multipart SMS is out of scope.
func DefaultRules() Rules {
	return Rules{
		MaxContentLength: map[Channel]int{
			ChannelSMS:   160,
			ChannelEmail: 10_000,
			ChannelPush:  256,
		},
		MaxBatchSize:            1000,
		MaxScheduleAhead:        30 * 24 * time.Hour,
		MaxAttempts:             5,
		MaxIdempotencyKeyLength: 255,
	}
}

const (
	maxEmailLength     = 254 // RFC 5321 path limit
	maxPushTokenLength = 4096
)

// e164 matches an E.164 phone number: '+', no leading zero, 8–15 digits in total.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func (r Rules) validateRecipient(ch Channel, recipient, prefix string, ve *ValidationError) {
	field := prefix + "recipient"
	if strings.TrimSpace(recipient) == "" {
		ve.add(field, "is required")
		return
	}
	switch ch {
	case ChannelSMS:
		if !e164.MatchString(recipient) {
			ve.add(field, "must be an E.164 phone number, e.g. +905551234567")
		}
	case ChannelEmail:
		if !isEmail(recipient) {
			ve.add(field, "must be a valid email address")
		}
	case ChannelPush:
		if len(recipient) > maxPushTokenLength || strings.IndexFunc(recipient, isSpaceOrControl) >= 0 {
			ve.add(field, "must be a device token without whitespace, at most "+itoa(maxPushTokenLength)+" bytes")
		}
	}
}

func (r Rules) validateContent(ch Channel, content, prefix string, ve *ValidationError) {
	field := prefix + "content"
	if strings.TrimSpace(content) == "" {
		ve.add(field, "is required")
		return
	}
	if !utf8.ValidString(content) {
		ve.add(field, "must be valid UTF-8")
		return
	}
	if limit := r.MaxContentLength[ch]; limit > 0 && utf8.RuneCountInString(content) > limit {
		ve.add(field, "must be at most "+itoa(limit)+" characters for "+string(ch))
	}
}

// isEmail accepts a bare address only ("a@b.com"), not "Name <a@b.com>".
func isEmail(s string) bool {
	if len(s) > maxEmailLength {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return strings.Contains(s[at+1:], ".")
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

func itoa(i int) string { return strconv.Itoa(i) }
