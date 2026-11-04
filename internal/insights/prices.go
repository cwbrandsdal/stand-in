package insights

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// API-equivalent cost prices each request at the provider's public API list price. Subscriptions
// are not billed per token; the figure shows what the same traffic would cost on the API.

//go:embed prices.json
var embeddedPricesJSON []byte

const (
	defaultPricesURL     = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	pricesRefreshEvery   = 24 * time.Hour
	pricesFirstRefresh   = time.Minute
	pricesRetryAfter     = time.Hour
	maxPricesBytes       = 32 << 20
	priceInput           = "input_cost_per_token"
	priceOutput          = "output_cost_per_token"
	priceCacheRead       = "cache_read_input_token_cost"
	priceCacheWrite      = "cache_creation_input_token_cost"
	backfillCostBatchMax = 5000
)

var (
	priceFieldPattern     = regexp.MustCompile(`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost)(_above_(\d+)k_tokens)?(_(priority|ultrafast|flex))?$`)
	priceDateSuffix       = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)
	priceModelNamePattern = regexp.MustCompile(`^(claude-|gpt-|o\d|codex-|chatgpt-)`)
	prices                atomic.Pointer[priceTable]
)

// modelPrice holds one model's per-token prices keyed by LiteLLM field name.
type modelPrice struct {
	rates map[string]float64
	// thresholds are the prompt sizes (tokens) above which "_above_<n>k_tokens" prices apply, descending.
	thresholds []int64
	// fastMultiplier scales Anthropic fast-mode requests; zero when the model has no fast mode.
	fastMultiplier float64
}

type priceTable struct {
	models map[string]*modelPrice
	source string
	loaded time.Time
}

// parsePriceTable reads a LiteLLM model price document, keeping Anthropic and OpenAI chat models.
func parsePriceTable(data []byte, source string) (*priceTable, error) {
	var document map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &document); errUnmarshal != nil {
		return nil, fmt.Errorf("parse prices: %w", errUnmarshal)
	}
	table := &priceTable{models: make(map[string]*modelPrice), source: source, loaded: time.Now()}
	for name, raw := range document {
		name = strings.ToLower(strings.TrimSpace(name))
		if strings.Contains(name, "/") || !priceModelNamePattern.MatchString(name) {
			continue
		}
		var entry map[string]any
		if errEntry := json.Unmarshal(raw, &entry); errEntry != nil {
			continue
		}
		if provider, _ := entry["litellm_provider"].(string); provider != "" && provider != "anthropic" && provider != "openai" {
			continue
		}
		price := &modelPrice{rates: make(map[string]float64)}
		seen := make(map[int64]bool)
		for key, value := range entry {
			match := priceFieldPattern.FindStringSubmatch(key)
			rate, isNumber := value.(float64)
			if match == nil || !isNumber || rate < 0 {
				continue
			}
			price.rates[key] = rate
			if match[3] != "" {
				if thousands, errParse := strconv.ParseInt(match[3], 10, 64); errParse == nil && !seen[thousands] {
					seen[thousands] = true
					price.thresholds = append(price.thresholds, thousands*1000)
				}
			}
		}
		if specific, ok := entry["provider_specific_entry"].(map[string]any); ok {
			if fast, isNumber := specific["fast"].(float64); isNumber && fast > 0 {
				price.fastMultiplier = fast
			}
		}
		// A model missing input or output prices would understate cost, so it stays unpriced.
		if _, hasInput := price.rates[priceInput]; !hasInput {
			continue
		}
		if _, hasOutput := price.rates[priceOutput]; !hasOutput {
			continue
		}
		sort.Slice(price.thresholds, func(i, j int) bool { return price.thresholds[i] > price.thresholds[j] })
		table.models[name] = price
	}
	if len(table.models) == 0 {
		return nil, fmt.Errorf("parse prices: no Anthropic or OpenAI models")
	}
	return table, nil
}

func (t *priceTable) lookup(model string) *modelPrice {
	if t == nil {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if _, rest, ok := strings.Cut(name, "/"); ok {
		name = rest
	}
	for _, candidate := range []string{name, strings.TrimSuffix(name, "-thinking"), priceDateSuffix.ReplaceAllString(name, "")} {
		if price, ok := t.models[candidate]; ok {
			return price
		}
	}
	return nil
}

// costUSD prices one request. tier is the effective service tier; ok is false for unknown models.
func (t *priceTable) costUSD(model, tier string, input, cacheRead, cacheWrite, output int64) (float64, bool) {
	price := t.lookup(model)
	if price == nil {
		return 0, false
	}
	prompt := input + cacheRead + cacheWrite
	above := ""
	for _, threshold := range price.thresholds {
		if prompt > threshold {
			above = "_above_" + strconv.FormatInt(threshold/1000, 10) + "k_tokens"
			break
		}
	}
	suffix, multiplier := "", 1.0
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "priority":
		suffix = "_priority"
	case "fast":
		// Codex calls priority "fast"; Anthropic prices fast mode as a multiple of the standard rate.
		if price.fastMultiplier > 0 {
			multiplier = price.fastMultiplier
		} else {
			suffix = "_priority"
		}
	case "ultrafast":
		suffix = "_ultrafast"
	case "flex":
		suffix = "_flex"
	}
	rate := func(field string) (float64, bool) {
		for _, key := range []string{field + above + suffix, field + above, field + suffix, field} {
			if value, ok := price.rates[key]; ok {
				return value, true
			}
		}
		return 0, false
	}
	inputRate, _ := rate(priceInput)
	outputRate, _ := rate(priceOutput)
	readRate, hasRead := rate(priceCacheRead)
	if !hasRead {
		readRate = inputRate
	}
	writeRate, hasWrite := rate(priceCacheWrite)
	if !hasWrite {
		writeRate = inputRate
	}
	cost := float64(input)*inputRate + float64(cacheRead)*readRate + float64(cacheWrite)*writeRate + float64(output)*outputRate
	return cost * multiplier, true
}

// cacheRates returns the standard cache write and read prices per token, for waste estimates.
func (t *priceTable) cacheRates(model string) (write, read float64, ok bool) {
	price := t.lookup(model)
	if price == nil {
		return 0, 0, false
	}
	input := price.rates[priceInput]
	write, read = input, input
	if value, has := price.rates[priceCacheWrite]; has {
		write = value
	}
	if value, has := price.rates[priceCacheRead]; has {
		read = value
	}
	return write, read, true
}

func currentPrices() *priceTable { return prices.Load() }

func init() {
	table, errParse := parsePriceTable(embeddedPricesJSON, "embedded")
	if errParse != nil {
		log.Errorf("insights: embedded prices: %v", errParse)
		return
	}
	prices.Store(table)
}

// requestCost prices a row with the current table.
func requestCost(row requestRow) (float64, bool) {
	return currentPrices().costUSD(row.Model, row.ServiceTier, row.InputTokens, row.CacheReadTokens, row.CacheWriteTokens, row.OutputTokens)
}

// pricesURL returns the refresh source; "off" keeps the embedded table.
func pricesURL() string {
	raw := strings.TrimSpace(os.Getenv("INSIGHTS_PRICES_URL"))
	if strings.EqualFold(raw, "off") {
		return ""
	}
	if raw == "" {
		return defaultPricesURL
	}
	return raw
}

// runPriceUpdater refreshes prices daily and prices rows recorded without a known price.
func (s *Service) runPriceUpdater(ctx context.Context) {
	s.backfillCosts(ctx)
	source := pricesURL()
	if source == "" {
		return
	}
	timer := time.NewTimer(pricesFirstRefresh)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := pricesRefreshEvery
		table, errFetch := fetchPriceTable(ctx, source)
		if errFetch != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warnf("insights: price refresh failed, keeping %s prices: %v", currentPrices().sourceName(), errFetch)
			next = pricesRetryAfter
		} else {
			added := table.keepMissing(currentPrices())
			prices.Store(table)
			log.Infof("insights: loaded prices for %d models from %s (%d kept from the previous table)", len(table.models), source, added)
			s.backfillCosts(ctx)
		}
		timer.Reset(next)
	}
}

// keepMissing copies models that t lacks from previous, so a refresh never drops a price the hub
// already had. It returns how many were copied.
func (t *priceTable) keepMissing(previous *priceTable) int {
	if previous == nil {
		return 0
	}
	added := 0
	for name, price := range previous.models {
		if _, ok := t.models[name]; !ok {
			t.models[name] = price
			added++
		}
	}
	return added
}

func (t *priceTable) sourceName() string {
	if t == nil {
		return "no"
	}
	return t.source
}

func fetchPriceTable(ctx context.Context, source string) (*priceTable, error) {
	var reader io.ReadCloser
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if errRequest != nil {
			return nil, errRequest
		}
		resp, errDo := http.DefaultClient.Do(req)
		if errDo != nil {
			return nil, errDo
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
		}
		reader = resp.Body
	} else {
		file, errOpen := os.Open(source)
		if errOpen != nil {
			return nil, errOpen
		}
		reader = file
	}
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.Debugf("insights: close price source: %v", errClose)
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(reader, maxPricesBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(data) > maxPricesBytes {
		return nil, fmt.Errorf("price document larger than %d bytes", maxPricesBytes)
	}
	return parsePriceTable(data, source)
}

// backfillCosts prices rows stored without a cost, for example before their model had a price.
// It walks the table in id order, one batch per transaction.
func (s *Service) backfillCosts(ctx context.Context) {
	table := currentPrices()
	if table == nil {
		return
	}
	var afterID int64
	var priced int
	for {
		count, lastID, scanned, errBatch := s.backfillCostBatch(ctx, table, afterID)
		if errBatch != nil {
			log.Warnf("insights: cost backfill: %v", errBatch)
			return
		}
		priced += count
		if scanned < backfillCostBatchMax {
			break
		}
		afterID = lastID
	}
	if priced > 0 {
		log.Infof("insights: priced %d earlier requests", priced)
	}
}

// backfillCostBatch prices one batch of unpriced rows with ids above afterID. It returns the number
// priced, the last id scanned and how many rows were scanned.
func (s *Service) backfillCostBatch(ctx context.Context, table *priceTable, afterID int64) (int, int64, int, error) {
	rows, errQuery := s.store.db.QueryContext(ctx, `SELECT id, COALESCE(model,''), COALESCE(service_tier,''),
			input_tokens, cache_read_tokens, cache_write_tokens, output_tokens
		FROM requests WHERE cost_usd IS NULL AND total_tokens > 0 AND id > ? ORDER BY id LIMIT ?`, afterID, backfillCostBatchMax)
	if errQuery != nil {
		return 0, afterID, 0, errQuery
	}
	type priced struct {
		id   int64
		cost float64
	}
	var updates []priced
	lastID, scanned := afterID, 0
	for rows.Next() {
		var id, input, cacheRead, cacheWrite, output int64
		var model, tier string
		if errScan := rows.Scan(&id, &model, &tier, &input, &cacheRead, &cacheWrite, &output); errScan != nil {
			_ = rows.Close()
			return 0, lastID, scanned, errScan
		}
		lastID = id
		scanned++
		if cost, ok := table.costUSD(model, tier, input, cacheRead, cacheWrite, output); ok {
			updates = append(updates, priced{id: id, cost: cost})
		}
	}
	if errClose := closeRows(rows); errClose != nil {
		return 0, lastID, scanned, errClose
	}
	if len(updates) == 0 {
		return 0, lastID, scanned, nil
	}
	tx, errBegin := s.store.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return 0, lastID, scanned, errBegin
	}
	for _, update := range updates {
		if _, errExec := tx.ExecContext(ctx, `UPDATE requests SET cost_usd = ? WHERE id = ?`, update.cost, update.id); errExec != nil {
			_ = tx.Rollback()
			return 0, lastID, scanned, errExec
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return 0, lastID, scanned, errCommit
	}
	return len(updates), lastID, scanned, nil
}
