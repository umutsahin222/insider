package domain

import "testing"

func TestParsePriority(t *testing.T) {
	tests := []struct {
		in      string
		want    Priority
		wantErr bool
	}{
		{"high", PriorityHigh, false},
		{"normal", PriorityNormal, false},
		{"", PriorityNormal, false},
		{"low", PriorityLow, false},
		{"HIGH", 0, true},
		{"urgent", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParsePriority(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPriorityOrdering(t *testing.T) {
	// The claim query relies on ORDER BY priority ASC serving high first.
	if PriorityHigh >= PriorityNormal || PriorityNormal >= PriorityLow {
		t.Fatal("priorities must sort high < normal < low")
	}
}

func TestStatusTransitions(t *testing.T) {
	all := []Status{StatusPending, StatusProcessing, StatusSent, StatusFailed, StatusCancelled}
	allowed := map[[2]Status]bool{
		{StatusPending, StatusProcessing}: true,
		{StatusPending, StatusCancelled}:  true,
		{StatusProcessing, StatusSent}:    true,
		{StatusProcessing, StatusFailed}:  true,
		{StatusProcessing, StatusPending}: true,
		{StatusFailed, StatusPending}:     true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := from.CanTransitionTo(to), allowed[[2]Status{from, to}]; got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestStatusIsTerminal(t *testing.T) {
	tests := map[Status]bool{
		StatusPending:    false,
		StatusProcessing: false,
		StatusFailed:     false, // can be retried manually
		StatusSent:       true,
		StatusCancelled:  true,
	}
	for s, want := range tests {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, want)
		}
	}
}

func TestParseChannelAndStatus(t *testing.T) {
	for _, c := range Channels {
		if got, err := ParseChannel(string(c)); err != nil || got != c {
			t.Errorf("ParseChannel(%q) = %q, %v", c, got, err)
		}
	}
	if _, err := ParseChannel("fax"); err == nil {
		t.Error("ParseChannel(fax) should fail")
	}
	if _, err := ParseStatus("sent"); err != nil {
		t.Errorf("ParseStatus(sent): %v", err)
	}
	if _, err := ParseStatus("done"); err == nil {
		t.Error("ParseStatus(done) should fail")
	}
}
