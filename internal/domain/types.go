package domain

import "fmt"

// Channel is a delivery channel.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

// Channels lists every supported channel; workers start one pool per entry.
var Channels = []Channel{ChannelSMS, ChannelEmail, ChannelPush}

// ParseChannel converts external input into a Channel.
func ParseChannel(s string) (Channel, error) {
	switch c := Channel(s); c {
	case ChannelSMS, ChannelEmail, ChannelPush:
		return c, nil
	}
	return "", fmt.Errorf("unknown channel %q", s)
}

// Priority orders work in the queue. Lower values are served first, which matches
// the `ORDER BY priority` used by the claim query.
type Priority int16

const (
	PriorityHigh   Priority = 0
	PriorityNormal Priority = 1
	PriorityLow    Priority = 2
)

// ParsePriority converts external input into a Priority. Empty input means normal.
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "high":
		return PriorityHigh, nil
	case "", "normal":
		return PriorityNormal, nil
	case "low":
		return PriorityLow, nil
	}
	return 0, fmt.Errorf("unknown priority %q", s)
}

func (p Priority) String() string {
	switch p {
	case PriorityHigh:
		return "high"
	case PriorityNormal:
		return "normal"
	case PriorityLow:
		return "low"
	}
	return fmt.Sprintf("Priority(%d)", int16(p))
}

// Status is the lifecycle state of a notification.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusSent       Status = "sent"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

// ParseStatus converts external input (e.g. a list filter) into a Status.
func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case StatusPending, StatusProcessing, StatusSent, StatusFailed, StatusCancelled:
		return st, nil
	}
	return "", fmt.Errorf("unknown status %q", s)
}

// transitions is the complete state machine:
//
//	pending    → processing (worker claim) | cancelled (user)
//	processing → sent | failed (permanent error / attempts exhausted)
//	           | pending (retry scheduled, or lease expired and reaped)
//	failed     → pending (manual retry)
//
// sent and cancelled are terminal.
var transitions = map[Status][]Status{
	StatusPending:    {StatusProcessing, StatusCancelled},
	StatusProcessing: {StatusSent, StatusFailed, StatusPending},
	StatusFailed:     {StatusPending},
}

// CanTransitionTo reports whether moving from s to next is allowed.
// Stores enforce the same rule with conditional UPDATEs (WHERE status = s).
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further transitions are possible.
func (s Status) IsTerminal() bool { return len(transitions[s]) == 0 }
