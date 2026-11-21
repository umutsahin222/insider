package domain

// SMS bodies are sent in one of two alphabets (3GPP TS 23.038):
//
//   - GSM-7, 7 bits per character: 160 characters fit in one SMS. A few characters
//     (^ { } [ ] ~ \ | € and form feed) live in the extension table and take two septets.
//   - UCS-2 (UTF-16), 16 bits per unit, as soon as one character is outside GSM-7:
//     70 units fit in one SMS. Characters outside the Basic Multilingual Plane (most
//     emoji) take two units.
//
// Longer texts are split into concatenated segments; each segment loses room to the
// concatenation header (153 septets or 67 units), and a two-unit character is never split
// across segments. National language shift tables (e.g. Turkish ş, ğ, ı in GSM-7) need
// provider support and are not used, so such text is measured as UCS-2, which is how it is
// sent without them.

// SMSEncoding is the alphabet an SMS body is sent in.
type SMSEncoding string

const (
	SMSEncodingGSM7 SMSEncoding = "GSM-7"
	SMSEncodingUCS2 SMSEncoding = "UCS-2"
)

const (
	gsm7SingleSegment = 160
	gsm7PerSegment    = 153
	ucs2SingleSegment = 70
	ucs2PerSegment    = 67
)

// gsm7Basic and gsm7Extension list the GSM-7 default alphabet and its extension table.
// 0x09 is the capital Ç, as in the Unicode GSM 03.38 mapping; the small ç is not GSM-7.
const (
	gsm7Basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
		"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsm7Extension = "\f^{}\\[~]|€"
)

// gsm7Septets maps every GSM-7 character to the septets it takes. Read-only after init.
var gsm7Septets = func() map[rune]int {
	m := make(map[rune]int, 140)
	for _, r := range gsm7Basic {
		m[r] = 1
	}
	for _, r := range gsm7Extension {
		m[r] = 2
	}
	return m
}()

// SMSLength describes how an SMS body is transmitted.
type SMSLength struct {
	Encoding SMSEncoding
	// Units are septets for GSM-7 and UTF-16 code units for UCS-2.
	Units    int
	Segments int
	// NonGSM is the first character that forced UCS-2; 0 for GSM-7 text.
	NonGSM rune
}

// MeasureSMS reports the encoding, size and number of segments of an SMS body.
func MeasureSMS(text string) SMSLength {
	m := SMSLength{Encoding: SMSEncodingGSM7}
	for _, r := range text {
		if _, ok := gsm7Septets[r]; !ok {
			m.Encoding, m.NonGSM = SMSEncodingUCS2, r
			break
		}
	}

	width, single, perSegment := smsSeptets, gsm7SingleSegment, gsm7PerSegment
	if m.Encoding == SMSEncodingUCS2 {
		width, single, perSegment = utf16Units, ucs2SingleSegment, ucs2PerSegment
	}
	for _, r := range text {
		m.Units += width(r)
	}
	switch {
	case m.Units == 0:
		return m
	case m.Units <= single:
		m.Segments = 1
		return m
	}

	// Fill segments in order; a character that does not fit whole starts the next one.
	m.Segments = 1
	used := 0
	for _, r := range text {
		w := width(r)
		if used+w > perSegment {
			m.Segments++
			used = 0
		}
		used += w
	}
	return m
}

// SMSCapacity is the most units a text in encoding e may have to fit in segments segments.
func SMSCapacity(e SMSEncoding, segments int) int {
	single, perSegment := gsm7SingleSegment, gsm7PerSegment
	if e == SMSEncodingUCS2 {
		single, perSegment = ucs2SingleSegment, ucs2PerSegment
	}
	if segments <= 1 {
		return single
	}
	return perSegment * segments
}

func smsSeptets(r rune) int { return gsm7Septets[r] }

func utf16Units(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}
