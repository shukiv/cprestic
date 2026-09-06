package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	gnizaFeature        = "gniza"
	cpanelWHMAPI        = "/usr/local/cpanel/bin/whmapi1"
	featureCacheTTL     = 30 * time.Second
	featureCheckTimeout = 5 * time.Second
	maxFeatureResponse  = 64 << 10
)

type featureDecision struct {
	allowed bool
	until   time.Time
}

// accountFeatureGate enforces Feature Manager behind the public Unix
// socket. The PHP frontend also checks it for a useful page-level refusal,
// but an account owns processes that can connect to the socket directly.
type accountFeatureGate struct {
	mu        sync.Mutex
	decisions map[string]featureDecision
	check     func(context.Context, string) (bool, error)
	slots     chan struct{}
}

func newAccountFeatureGate() accountFeatureGate {
	return accountFeatureGate{
		decisions: map[string]featureDecision{},
		check:     cpanelFeatureEnabled,
		// Do not let many distinct local UIDs turn the root service into an
		// unbounded whmapi1 process launcher on a large shared server.
		slots: make(chan struct{}, 8),
	}
}

func (g *accountFeatureGate) allowed(ctx context.Context, account string) (bool, error) {
	now := time.Now()
	g.mu.Lock()
	decision, found := g.decisions[account]
	g.mu.Unlock()
	if found && now.Before(decision.until) {
		return decision.allowed, nil
	}

	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}

	allowed, err := g.check(ctx, account)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	g.decisions[account] = featureDecision{allowed: allowed, until: now.Add(featureCacheTTL)}
	g.mu.Unlock()
	return allowed, nil
}

// cpanelFeatureEnabled asks cPanel itself instead of reimplementing feature
// list inheritance and the server-wide disabled list. Arguments are passed
// without a shell and account names have already come from cPanel's account
// registry via SO_PEERCRED.
func cpanelFeatureEnabled(ctx context.Context, account string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, featureCheckTimeout)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, cpanelWHMAPI, "--output=json",
		"verify_user_has_feature", "user="+account, "feature="+gnizaFeature).Output()
	if err != nil {
		return false, fmt.Errorf("check cPanel feature access for %s: %w", account, err)
	}
	if len(output) > maxFeatureResponse {
		return false, fmt.Errorf("check cPanel feature access for %s: response is too large", account)
	}
	return parseFeatureResponse(output)
}

func parseFeatureResponse(output []byte) (bool, error) {
	var response struct {
		Data struct {
			HasFeature int `json:"has_feature"`
		} `json:"data"`
		Metadata struct {
			Result int    `json:"result"`
			Reason string `json:"reason"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return false, fmt.Errorf("decode cPanel feature response: %w", err)
	}
	if response.Metadata.Result != 1 {
		if response.Metadata.Reason == "" {
			response.Metadata.Reason = "cPanel did not accept the feature query"
		}
		return false, fmt.Errorf("cPanel feature query failed: %s", response.Metadata.Reason)
	}
	return response.Data.HasFeature == 1, nil
}
