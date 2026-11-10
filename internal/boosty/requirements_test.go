package boosty

import (
	"encoding/json"
	"testing"
)

func TestPostTierIsDistinctFromViewerSubscription(t *testing.T) {
	cases := []struct {
		raw      string
		wantTier string
		wantBuy  float64
	}{
		{`{"subscriptionLevel":{"name":"VIP","price":499},"price":0}`, "VIP", 0},
		{`{"subscriptionLevel":null,"price":199}`, "", 199},
		{`{"subscriptionLevel":"not-an-object","price":0}`, "", 0},
		{`{"subscriptionLevel":{"name":"Tier C","price":350},"price":25}`, "Tier C", 25},
		{`{"title":"public post"}`, "", 0},
	}
	for _, tt := range cases {
		got := ParsePostRequirement(json.RawMessage(tt.raw))
		if tt.wantTier == "" && tt.wantBuy == 0 {
			if got != nil {
				t.Fatalf("unverified post tier invented: %+v", got)
			}
			continue
		}
		if got == nil || got.TierName != tt.wantTier || got.SinglePurchase != tt.wantBuy || got.Source != "boosty_post" {
			t.Fatalf("wrong Boosty post tier: %+v for %s", got, tt.raw)
		}
	}
}
