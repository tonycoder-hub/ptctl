package tjupt

import "testing"

// Site ID, origins, and route IDs are persisted in metastore records, sealed
// site bindings, and bonus exchange journals. Changing any of them strands
// existing records, so a new route shape must get a new ".vN" ID instead.
func TestPersistedIdentityIsFrozen(t *testing.T) {
	frozen := map[string][2]string{
		"SiteID":                  {SiteID, "tjupt"},
		"Origin":                  {Origin, "https://www.tjupt.org"},
		"DefaultBaseURL":          {DefaultBaseURL, "https://www.tjupt.org/"},
		"DetailOrigin":            {DetailOrigin, "https://www.tjupt.org"},
		"MetafileOrigin":          {MetafileOrigin, "https://www.tjupt.org"},
		"BonusReviewOrigin":       {BonusReviewOrigin, "https://www.tjupt.org"},
		"DetailRouteID":           {DetailRouteID, "tjupt.details_by_id_no_hit.v1"},
		"MetafileRouteID":         {MetafileRouteID, "tjupt.download_by_id.v1"},
		"BonusReviewRouteID":      {BonusReviewRouteID, "tjupt.mybonusapps_offer_review.v1"},
		"BonusExchangeRouteID":    {BonusExchangeRouteID, "tjupt.mybonusapps_exchange_post.v1"},
		"bonusUnknownActionRoute": {bonusUnknownActionRoute, "tjupt.bonus_action_unrecognized.v1"},
	}
	for name, pair := range frozen {
		if pair[0] != pair[1] {
			t.Errorf("%s=%q, want frozen %q", name, pair[0], pair[1])
		}
	}
}

func TestGuardedClientFactoryReturnsUntypedNilOnError(t *testing.T) {
	client, err := newGuardedClient("http://insecure.example/", "sid=secret", 0)
	if err == nil || client != nil {
		t.Fatalf("client=%#v err=%v", client, err)
	}
}

func TestOnlyTheExactDefaultBaseURLIsProduction(t *testing.T) {
	for _, baseURL := range []string{"", DefaultBaseURL} {
		if !New(baseURL).productionOrigin() {
			t.Fatalf("%q not treated as production", baseURL)
		}
	}
	for _, baseURL := range []string{Origin, "https://WWW.tjupt.org/", "https://www.tjupt.org:443/", "https://mirror.example/"} {
		if New(baseURL).productionOrigin() {
			t.Fatalf("%q treated as production", baseURL)
		}
	}
	var missing *Adapter
	if missing.productionOrigin() {
		t.Fatal("nil adapter treated as production")
	}
}
