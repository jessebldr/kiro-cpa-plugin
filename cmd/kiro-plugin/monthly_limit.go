package main

import (
	"path/filepath"
	"strings"
	"time"

	kiroauth "github.com/jessebldr/kiro-cpa-plugin/internal/auth/kiro"
)

// monthlyQuotaExhausted reports whether a live usage reading shows a credential
// that Kiro will refuse with 402: a plan bucket with a limit and nothing left,
// overage not enabled, and no trial, bonus or overage-credit pool with anything
// left to spend. A reading without a plan limit says nothing, so it is never
// taken as exhaustion. reset is the plan bucket's own reset time, empty when
// Kiro gave none.
func monthlyQuotaExhausted(account usageAccountView) (exhausted bool, reset string) {
	if account.OverageStatus == "enabled" {
		return false, ""
	}
	for _, bucket := range account.Buckets {
		if bucket.Kind != usageKindPlan && bucket.Remaining > 0 {
			return false, ""
		}
	}
	for _, bucket := range account.Buckets {
		if bucket.Kind == usageKindPlan && bucket.Limit > 0 {
			if bucket.Remaining > 0 {
				return false, ""
			}
			exhausted, reset = true, bucket.Reset
		}
	}
	return exhausted, reset
}

// usageAuthID is the id the executor keys a credential by: CPA's auth id,
// which for a file credential is the file's name. quota.fetch knows a
// credential only by its auth index, so the host's file name stands in for an
// id the entry does not carry.
func usageAuthID(credential usageCredential) string {
	if id := strings.TrimSpace(credential.entry.ID); id != "" {
		return id
	}
	if name := strings.TrimSpace(credential.entry.Name); name != "" {
		return filepath.Base(name)
	}
	return strings.TrimSpace(filepath.Base(credential.authRecord.Name))
}

// syncMonthlyLimitParking carries a live usage reading into the rate limiter,
// so a credential with no quota left leaves rotation before a user request
// collects a 402 from it, and returns once quota shows again. It parks until
// the reset Kiro reports, or the next UTC month when that is missing or past.
// A credential that opted out of cooling is left alone, as in the executor.
func syncMonthlyLimitParking(credential usageCredential, account usageAccountView) {
	authID := usageAuthID(credential)
	if authID == "" || (credential.token != nil && credential.token.DisableCooling != nil && *credential.token.DisableCooling) {
		return
	}
	limiter := kiroauth.GetGlobalRateLimiter()
	exhausted, reset := monthlyQuotaExhausted(account)
	if !exhausted {
		limiter.ClearUnavailable(authID, kiroauth.ReasonMonthlyLimit)
		return
	}
	window := time.Until(parseTime(reset))
	if window <= 0 {
		window = kiroauth.UntilNextUTCMonth(time.Now())
	}
	limiter.MarkUnavailable(authID, kiroauth.ReasonMonthlyLimit, window)
}
