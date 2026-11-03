package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// codexSpeedTiersURL is the catalog native Codex clients read when signed in with ChatGPT.
	// Speed tiers do not depend on the client version; it only has to be one the backend serves.
	codexSpeedTiersURL        = "https://chatgpt.com/backend-api/codex/models?client_version=0.166.0"
	codexSpeedTiersOriginator = "codex-tui"
	codexSpeedTiersUserAgent  = "codex-tui/0.166.0"
	codexSpeedTiersFirstDelay = 15 * time.Second
	codexSpeedTiersInterval   = time.Hour
	codexSpeedTiersRetry      = 5 * time.Minute
	maxCodexSpeedTiersBytes   = 8 << 20
)

// startCodexSpeedTierSync keeps the Codex client catalog's speed tiers in line with ChatGPT's
// own catalog (see codexmodels.SetLiveSpeedTiers). It reads ChatGPT's catalog with each enabled
// Codex OAuth credential at start and then hourly; failures keep the last good tiers.
func (s *Service) startCodexSpeedTierSync(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	go func() {
		timer := time.NewTimer(codexSpeedTiersFirstDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			next := codexSpeedTiersRetry
			if s.refreshCodexSpeedTiers(ctx) {
				next = codexSpeedTiersInterval
			}
			timer.Reset(next)
		}
	}()
}

func (s *Service) refreshCodexSpeedTiers(ctx context.Context) bool {
	var catalogs [][]byte
	for _, auth := range s.coreManager.List() {
		if !codexSpeedTiersEligible(auth) {
			continue
		}
		catalog, errFetch := s.fetchCodexSpeedTierCatalog(ctx, auth)
		if errFetch != nil {
			if ctx.Err() == nil {
				log.Warnf("codex speed tiers: credential %s: %v", auth.ID, errFetch)
			}
			continue
		}
		catalogs = append(catalogs, catalog)
	}
	if len(catalogs) == 0 {
		return false
	}
	models, errSet := codexmodels.SetLiveSpeedTiers(catalogs)
	if errSet != nil {
		log.Warnf("codex speed tiers: %v", errSet)
		return false
	}
	log.Infof("codex speed tiers: synced %d models from %d credential(s)", models, len(catalogs))
	return true
}

// codexSpeedTiersEligible accepts enabled Codex credentials signed in with ChatGPT. API-key
// credentials use the OpenAI API, whose catalog this is not.
func codexSpeedTiersEligible(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token) != ""
}

func (s *Service) fetchCodexSpeedTierCatalog(ctx context.Context, auth *coreauth.Auth) ([]byte, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, codexSpeedTiersURL, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", codexSpeedTiersOriginator)
	req.Header.Set("User-Agent", codexSpeedTiersUserAgent)
	if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	// The executor adds the credential's bearer token and configured custom headers.
	resp, errDo := s.coreManager.HttpRequest(ctx, auth, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("codex speed tiers: close response: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ChatGPT catalog HTTP status %d", resp.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxCodexSpeedTiersBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(body) > maxCodexSpeedTiersBytes {
		return nil, fmt.Errorf("ChatGPT catalog larger than %d bytes", maxCodexSpeedTiersBytes)
	}
	return body, nil
}
