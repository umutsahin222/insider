package domain

import (
	"strings"
	"testing"
)

func TestMeasureSMS(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		encoding SMSEncoding
		units    int
		segments int
		nonGSM   rune
	}{
		{"empty", "", SMSEncodingGSM7, 0, 0, 0},
		{"plain ascii", "Your code is 1234", SMSEncodingGSM7, 17, 1, 0},
		{"gsm-7 accents and greek", "Ça va? Δ è ñ ß £", SMSEncodingGSM7, 16, 1, 0},
		{"gsm-7 full segment", strings.Repeat("a", 160), SMSEncodingGSM7, 160, 1, 0},
		{"gsm-7 one over splits into 153-septet segments", strings.Repeat("a", 161), SMSEncodingGSM7, 161, 2, 0},
		{"gsm-7 two full parts", strings.Repeat("a", 306), SMSEncodingGSM7, 306, 2, 0},
		{"gsm-7 third part", strings.Repeat("a", 307), SMSEncodingGSM7, 307, 3, 0},
		{"extension characters take two septets", strings.Repeat("€", 80), SMSEncodingGSM7, 160, 1, 0},
		// 306 septets would be two full segments, but the € straddling the boundary moves on.
		{"extension character never split across segments", strings.Repeat("a", 152) + "€" + strings.Repeat("a", 152), SMSEncodingGSM7, 306, 3, 0},
		{"one extension character past the limit", strings.Repeat("a", 159) + "{", SMSEncodingGSM7, 161, 2, 0},
		{"small c cedilla is not gsm-7", "ç", SMSEncodingUCS2, 1, 1, 'ç'},
		{"turkish letters force ucs-2", "Kargonuz yola çıktı", SMSEncodingUCS2, 19, 1, 'ç'},
		{"ucs-2 full segment", strings.Repeat("ş", 70), SMSEncodingUCS2, 70, 1, 'ş'},
		{"ucs-2 one over splits into 67-unit segments", strings.Repeat("ş", 71), SMSEncodingUCS2, 71, 2, 'ş'},
		{"ucs-2 counts every character once switched", strings.Repeat("a", 69) + "ğ", SMSEncodingUCS2, 70, 1, 'ğ'},
		{"emoji take two units", strings.Repeat("😀", 35), SMSEncodingUCS2, 70, 1, '😀'},
		// 134 units would be two full segments, but the surrogate pair straddling the boundary moves on.
		{"emoji never split across segments", strings.Repeat("a", 66) + "😀" + strings.Repeat("a", 66), SMSEncodingUCS2, 134, 3, '😀'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MeasureSMS(tt.text)
			want := SMSLength{Encoding: tt.encoding, Units: tt.units, Segments: tt.segments, NonGSM: tt.nonGSM}
			if got != want {
				t.Errorf("MeasureSMS = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSMSCapacity(t *testing.T) {
	tests := []struct {
		encoding SMSEncoding
		segments int
		want     int
	}{
		{SMSEncodingGSM7, 1, 160},
		{SMSEncodingGSM7, 3, 459},
		{SMSEncodingUCS2, 1, 70},
		{SMSEncodingUCS2, 3, 201},
	}
	for _, tt := range tests {
		if got := SMSCapacity(tt.encoding, tt.segments); got != tt.want {
			t.Errorf("SMSCapacity(%s, %d) = %d, want %d", tt.encoding, tt.segments, got, tt.want)
		}
	}
}

func TestSMSLengthLimit(t *testing.T) {
	multi := DefaultRules()
	multi.MaxSMSSegments = 3

	tests := []struct {
		name    string
		rules   Rules
		content string
		valid   bool
		message string // substring of the error message
	}{
		{"one gsm-7 segment", DefaultRules(), strings.Repeat("a", 160), true, ""},
		{"gsm-7 over one segment", DefaultRules(), strings.Repeat("a", 161), false, "at most 160 GSM-7 characters"},
		{"one ucs-2 segment", DefaultRules(), strings.Repeat("ş", 70), true, ""},
		{"ucs-2 over one segment names the character", DefaultRules(), "Siparişiniz " + strings.Repeat("x", 60), false, "'ş' is not in the GSM-7 alphabet"},
		{"three segments allowed", multi, strings.Repeat("a", 459), true, ""},
		{"over three segments", multi, strings.Repeat("a", 460), false, "3 SMS segments: at most 459"},
		{"three ucs-2 segments allowed", multi, strings.Repeat("ı", 201), true, ""},
		{"over three ucs-2 segments", multi, strings.Repeat("ı", 202), false, "at most 201 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validSMS()
			in.Content = tt.content
			_, err := NewNotification(in, tt.rules, now)
			if tt.valid {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if f := fields(t, err); len(f) != 1 || f[0] != "content" {
				t.Fatalf("fields = %v, want [content]", f)
			}
			if !strings.Contains(err.(*ValidationError).Errors[0].Message, tt.message) {
				t.Errorf("message = %q, want it to contain %q", err.(*ValidationError).Errors[0].Message, tt.message)
			}
		})
	}
}
