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
	// MaxContentLength is counted in characters (runes), per channel. SMS is limited by
	// MaxSMSSegments instead, because its capacity depends on the alphabet (see MeasureSMS).
	MaxContentLength map[Channel]int
	// MaxSMSSegments is how many concatenated segments one SMS may take.
	MaxSMSSegments          int
	MaxBatchSize            int
	MaxScheduleAhead        time.Duration
	MaxAttempts             int
	MaxIdempotencyKeyLength int
}

// DefaultRules returns the limits used when no configuration overrides them.
//
// An SMS must fit in one segment by default: 160 GSM-7 characters, or 70 when the text needs
// UCS-2. One notification is then always one billed message.
func DefaultRules() Rules {
	return Rules{
		MaxContentLength: map[Channel]int{
			ChannelEmail: 10_000,
			ChannelPush:  256,
		},
		MaxSMSSegments:          1,
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
	if ch == ChannelSMS {
		r.validateSMSLength(content, field, ve)
		return
	}
	if limit := r.MaxContentLength[ch]; limit > 0 && utf8.RuneCountInString(content) > limit {
		ve.add(field, "must be at most "+itoa(limit)+" characters for "+string(ch))
	}
}

// validateSMSLength rejects a text that needs more than MaxSMSSegments segments, saying
// which alphabet applies and why, so the caller can shorten or rephrase it.
func (r Rules) validateSMSLength(content, field string, ve *ValidationError) {
	limit := max(r.MaxSMSSegments, 1)
	m := MeasureSMS(content)
	if m.Segments <= limit {
		return
	}
	fit := "one SMS segment"
	if limit > 1 {
		fit = itoa(limit) + " SMS segments"
	}
	capacity := itoa(SMSCapacity(m.Encoding, limit))
	if m.Encoding == SMSEncodingGSM7 {
		ve.add(field, "must fit in "+fit+": at most "+capacity+" GSM-7 characters, where ^ { } [ ] ~ \\ | € count as two"+
			" (this text needs "+itoa(m.Segments)+" segments)")
		return
	}
	ve.add(field, "must fit in "+fit+": "+strconv.QuoteRune(m.NonGSM)+" is not in the GSM-7 alphabet, so the text is sent"+
		" as UCS-2 with at most "+capacity+" characters (this text needs "+itoa(m.Segments)+" segments)")
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
