package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	kiroauth "github.com/jessebldr/kiro-cpa-plugin/internal/auth/kiro"
)

func TestMonthlyQuotaExhaustedReadsOnlyAnEmptyPlanWithNothingElseToSpend(t *testing.T) {
	empty := usageBucketView{Kind: usageKindPlan, Limit: 50, Used: 50, Remaining: 0, Reset: "2026-12-01T00:00:00Z"}
	for _, test := range []struct {
		name    string
		account usageAccountView
		want    bool
	}{
		{"plan empty", usageAccountView{Buckets: []usageBucketView{empty}}, true},
		{"plan with quota left", usageAccountView{Buckets: []usageBucketView{{Kind: usageKindPlan, Limit: 50, Used: 10, Remaining: 40}}}, false},
		{"overage enabled", usageAccountView{OverageStatus: "enabled", Buckets: []usageBucketView{empty}}, false},
		{"trial left", usageAccountView{Buckets: []usageBucketView{empty, {Kind: usageKindTrial, Limit: 500, Remaining: 450}}}, false},
		{"bonus left", usageAccountView{Buckets: []usageBucketView{empty, {Kind: usageKindBonus, Remaining: 100}}}, false},
		{"overage credit left", usageAccountView{Buckets: []usageBucketView{empty, {Kind: usageKindOverageCredit, Remaining: 5}}}, false},
		{"no plan limit reported", usageAccountView{Buckets: []usageBucketView{{Kind: usageKindPlan}}}, false},
		{"no buckets", usageAccountView{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, _ := monthlyQuotaExhausted(test.account); got != test.want {
				t.Fatalf("monthlyQuotaExhausted = %v, want %v", got, test.want)
			}
		})
	}
}

// quotaServer answers every usage request with the body held in *body.
func quotaServer(t *testing.T, body *string) {
	t.Helper()
	var mu sync.Mutex
	usageHTTPClient = func(string) httpDoer {
		return httpDoerFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(*body))}, nil
		})
	}
}

func usageBody(used int, reset string, extra string) string {
	field := ""
	if reset != "" {
		field = `,"nextDateReset":"` + reset + `"`
	}
	return fmt.Sprintf(`{"subscriptionInfo":{"subscriptionTitle":"KIRO FREE"},"usageBreakdownList":[{"displayNamePlural":"Credits","currentUsageWithPrecision":%d,"usageLimitWithPrecision":50%s%s}]}`, used, field, extra)
}

// A quota check that sees no plan credit left takes the credential out of
// rotation until the reset Kiro reports, before any user request collects a
// 402 from it, and a later reading with quota brings it back. The credential
// is keyed by the host's auth file name here because the fixture has no id.
func TestLiveZeroQuotaParksTheCredentialUntilResetAndClearsWhenQuotaReturns(t *testing.T) {
	usageFixture(t)
	limiter := kiroauth.GetGlobalRateLimiter()
	const authID = "kiro-a.json"
	limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit)
	t.Cleanup(func() { limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit) })

	reset := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	body := usageBody(50, reset.Format(time.RFC3339), "")
	quotaServer(t, &body)

	if _, err := fetchQuotaAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	reason, retryAfter, parked := limiter.TokenUnavailable(authID)
	if !parked || reason != kiroauth.ReasonMonthlyLimit {
		t.Fatalf("credential with 0 credits left: parked=%v reason=%q, want parked for the monthly limit", parked, reason)
	}
	if want := time.Until(reset); retryAfter > want+time.Minute || retryAfter < want-time.Minute {
		t.Fatalf("parked for %v, want until the reported reset (%v)", retryAfter, want)
	}
	if _, _, other := limiter.TokenUnavailable("kiro-b.json"); other {
		t.Fatal("an unrelated credential was parked")
	}

	body = usageBody(10, reset.Format(time.RFC3339), "")
	usageNow = func() time.Time { return time.Date(2026, 9, 25, 12, 5, 0, 0, time.UTC) }
	if _, err := fetchQuotaAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, parked := limiter.TokenUnavailable(authID); parked {
		t.Fatal("credential stayed parked after live usage showed quota again")
	}
}

func TestLiveZeroQuotaWithoutAResetParksUntilTheNextUTCMonth(t *testing.T) {
	usageFixture(t)
	limiter := kiroauth.GetGlobalRateLimiter()
	const authID = "kiro-a.json"
	t.Cleanup(func() { limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit) })
	body := usageBody(50, "", "")
	quotaServer(t, &body)

	before := time.Now()
	if _, err := fetchQuotaAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	_, retryAfter, parked := limiter.TokenUnavailable(authID)
	if !parked {
		t.Fatal("credential with 0 credits left and no reset was not parked")
	}
	if max, min := kiroauth.UntilNextUTCMonth(before), kiroauth.UntilNextUTCMonth(time.Now()); retryAfter > max || retryAfter < min-time.Minute {
		t.Fatalf("parked for %v, want the time to the next UTC month (%v..%v)", retryAfter, min, max)
	}
}

func TestLiveZeroQuotaLeavesAnOverageCreditCredentialInRotation(t *testing.T) {
	usageFixture(t)
	limiter := kiroauth.GetGlobalRateLimiter()
	const authID = "kiro-a.json"
	t.Cleanup(func() { limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit) })
	body := usageBody(50, "", `,"bonuses":[{"amount":20}]`)
	quotaServer(t, &body)

	if _, err := fetchQuotaAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, parked := limiter.TokenUnavailable(authID); parked {
		t.Fatal("credential with a bonus pool left was parked")
	}
}

func TestLiveUsageDoesNotLiftAnotherReason(t *testing.T) {
	usageFixture(t)
	limiter := kiroauth.GetGlobalRateLimiter()
	const authID = "kiro-a.json"
	limiter.MarkSuspended(authID)
	t.Cleanup(func() { limiter.ClearUnavailable(authID, kiroauth.ReasonSuspended) })
	body := usageBody(1, "", "")
	quotaServer(t, &body)

	if _, err := fetchQuotaAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if reason, _, parked := limiter.TokenUnavailable(authID); !parked || reason != kiroauth.ReasonSuspended {
		t.Fatalf("a usage reading with quota lifted the suspension: parked=%v reason=%q", parked, reason)
	}
}

// A credential parked for its monthly limit answers the listing with 403 on
// every refresh. With a stored catalogue the listing is skipped; without one
// it is still attempted, so a genuine auth error stays visible.
func TestRefreshSkipsRelistingForParkedCredentialWithStoredCatalogue(t *testing.T) {
	original := authRefreshCredential
	t.Cleanup(func() { authRefreshCredential = original })
	authRefreshCredential = func(_ context.Context, token *kiroauth.KiroTokenData) (*kiroauth.KiroTokenData, error) {
		refreshed := &kiroauth.KiroTokenData{AccessToken: "renewed", RefreshToken: "refresh", AuthMethod: "builder-id", Region: "us-east-1",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		carryHostOwnedFields(token, refreshed)
		return refreshed, nil
	}
	calls := stubCatalog(t, func(*http.Request) (*http.Response, error) {
		return catalogResponse(http.StatusForbidden, `{"message":"forbidden"}`), nil
	})
	limiter := kiroauth.GetGlobalRateLimiter()
	const authID = "kiro.json" // the id refreshAuth sends
	t.Cleanup(func() { limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit) })

	withCatalogue := credential(t, "access", time.Now().Add(5*time.Minute), true)
	refreshAuth(t, withCatalogue)
	if *calls != 1 {
		t.Fatalf("credential in rotation made %d listing calls, want 1", *calls)
	}

	limiter.MarkUnavailable(authID, kiroauth.ReasonMonthlyLimit, time.Hour)
	if got := catalogOf(t, refreshAuth(t, withCatalogue).Auth.StorageJSON); got != "claude-haiku-4.5" {
		t.Fatalf("catalogue after a skipped relisting = %q, want the stored one", got)
	}
	if *calls != 1 {
		t.Fatalf("parked credential with a stored catalogue made %d listing calls, want none", *calls-1)
	}

	refreshAuth(t, credential(t, "access", time.Now().Add(5*time.Minute), false))
	if *calls != 2 {
		t.Fatalf("parked credential without a catalogue made %d listing calls, want it still listed", *calls-1)
	}
}
