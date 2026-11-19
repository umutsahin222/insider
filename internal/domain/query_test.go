package domain

import "testing"

func TestDeliveryStats_SuccessRate(t *testing.T) {
	if _, ok := (DeliveryStats{}).SuccessRate(); ok {
		t.Error("no attempts must report ok=false, not a 0% success rate")
	}
	if r, ok := (DeliveryStats{Attempts: 8, Succeeded: 6}).SuccessRate(); !ok || r != 0.75 {
		t.Errorf("rate = %v, %v; want 0.75", r, ok)
	}
}
