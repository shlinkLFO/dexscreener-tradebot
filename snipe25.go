package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	// No longer need solana-go or the old price cache for this approach
)

const (
	// DexScreener API endpoint for searching pairs
	dexScreenerSearchAPI = "https://api.dexscreener.com/latest/dex/search"
	// Chain ID for Solana on DexScreener
	solanaChainID = "solana"
	// How often to refresh the data
	refreshInterval = 30 * time.Second // Refresh every 60 seconds
	// Number of top movers to display
	topMoversCount = 20
	// Minimum USD liquidity threshold to consider a pair
	minLiquidityUSD = 1000.0
	// Minimum 5-minute volume threshold (in USD)
	minVolume5mUSD = 100.0
	// Quote tokens we typically trade against (to identify the target token)
	commonQuoteSymbols = "SOL,USDC,USDT"
)

// --- DexScreener API Response Structures ---

type DexScreenerResponse struct {
	SchemaVersion string `json:"schemaVersion"`
	Pairs         []Pair `json:"pairs"`
	// Might be nil if no pairs found or error
}

type Pair struct {
	ChainID     string      `json:"chainId"`
	DexID       string      `json:"dexId"`
	URL         string      `json:"url"`
	PairAddress string      `json:"pairAddress"`
	BaseToken   Token       `json:"baseToken"`
	QuoteToken  Token       `json:"quoteToken"`
	PriceNative string      `json:"priceNative"` // Price in terms of quote token
	PriceUsd    string      `json:"priceUsd"`    // Can be null if issue fetching USD price
	Txns        Transactions `json:"txns"`
	Volume      Volume      `json:"volume"`
	PriceChange PriceChange `json:"priceChange"`
	Liquidity   Liquidity   `json:"liquidity"`
	Fdv         float64     `json:"fdv"` // Fully diluted valuation
	PairCreatedAt int64     `json:"pairCreatedAt"`
}

type Token struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Symbol  string `json:"symbol"`
}

type Transactions struct {
	M5 BuysSells `json:"m5"`
	H1 BuysSells `json:"h1"`
	H6 BuysSells `json:"h6"`
	H24 BuysSells `json:"h24"`
}

type BuysSells struct {
	Buys  int `json:"buys"`
	Sells int `json:"sells"`
}

type Volume struct {
	// Volume in USD
	H24 float64 `json:"h24"`
	H6  float64 `json:"h6"`
	H1  float64 `json:"h1"`
	M5  float64 `json:"m5"`
}

type PriceChange struct {
	// Percentage change
	M5  float64 `json:"m5"`
	H1  float64 `json:"h1"`
	H6  float64 `json:"h6"`
	H24 float64 `json:"h24"`
}

type Liquidity struct {
	Usd   float64 `json:"usd"` // Total liquidity in USD, might be null
	Base  float64 `json:"base"`
	Quote float64 `json:"quote"`
}

// --- Enhanced Structure for Our Use ---

type TokenMomentumInfo struct {
	PairAddress     string
	BaseTokenSymbol string
	BaseTokenAddr   string
	QuoteTokenSymbol string
	PriceChangeM5   float64 // 5m % change
	VolumeM5        float64 // 5m volume in USD
	LiquidityUSD    float64 // Current liquidity in USD
	PriceUSD        string  // Current price in USD
	PairURL         string
}

// Fetches pairs from DexScreener based on a search query
func fetchDexScreenerPairs(query string) ([]Pair, error) {
	// Construct the URL: search for the query term on the solana chain
	// DexScreener search seems to implicitly filter by chain based on common terms like 'SOL'
	// or you might filter client-side. Let's search for 'SOL' which usually brings up SOL pairs.
	// A more specific query might be needed depending on results.
	url := fmt.Sprintf("%s?q=%s+%s", dexScreenerSearchAPI, query, solanaChainID) // Try adding chain ID to query
	log.Printf("⏳ Fetching DexScreener data: %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error fetching from DexScreener: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to fetch from DexScreener: status code %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading DexScreener response body: %w", err)
	}

	// Handle potentially empty or non-JSON success responses
	if len(bodyBytes) == 0 {
		log.Println("⚠️ DexScreener returned empty body with 200 OK status.")
		return []Pair{}, nil
	}
	// log.Printf("DEBUG: DexScreener Raw Response: %s", string(bodyBytes)) // Keep for debugging if needed


	var apiResponse DexScreenerResponse
	if err := json.Unmarshal(bodyBytes, &apiResponse); err != nil {
		// Try unmarshalling into just []Pair if the top-level struct fails
		// Sometimes APIs return just the array directly
		var pairsDirect []Pair
		if errDirect := json.Unmarshal(bodyBytes, &pairsDirect); errDirect == nil {
			log.Println("ℹ️ Decoded DexScreener response as direct array.")
			return pairsDirect, nil
		}
		// If both fail, return the original error
		return nil, fmt.Errorf("error decoding DexScreener JSON: %w. Body was: %s", err, string(bodyBytes))
	}

	if apiResponse.Pairs == nil {
		log.Println("⚠️ DexScreener response contained no pairs array (or it was null).")
		return []Pair{}, nil // Return empty slice, not an error
	}

	log.Printf("✅ Found %d pairs from DexScreener search.", len(apiResponse.Pairs))
	return apiResponse.Pairs, nil
}

// The main scanning logic, designed to be called repeatedly
func runScan() {
	log.Println("--- Starting Scan Cycle ---")

	// 1. Fetch pairs from DexScreener (Searching for SOL pairs on Solana)
	// You might need to adjust the query ("SOL", "USDC", etc.) based on what works best
	pairs, err := fetchDexScreenerPairs("SOL")
	if err != nil {
		log.Printf("❌ Error fetching pairs: %v. Skipping cycle.", err)
		return // Skip rest of the cycle on error
	}

	if len(pairs) == 0 {
		log.Println("🤷 No pairs found in DexScreener response for the query. Skipping cycle.")
		return
	}

	// 2. Process and Filter Pairs
	var momentumCandidates []TokenMomentumInfo
	quoteSymbolsMap := make(map[string]bool)
	for _, s := range strings.Split(commonQuoteSymbols, ",") {
		quoteSymbolsMap[strings.TrimSpace(s)] = true
	}


	for _, pair := range pairs {
		// Basic sanity checks
		if pair.ChainID != solanaChainID {
			continue // Ensure it's actually Solana
		}
		if pair.BaseToken.Address == "" || pair.QuoteToken.Address == "" {
			continue // Skip pairs with missing token info
		}

        // We are interested in the momentum of the BASE token typically when QUOTE is SOL/USDC/USDT
		// Or momentum of QUOTE token if BASE is SOL/USDC/USDT. Let's focus on the first case.
		if !quoteSymbolsMap[pair.QuoteToken.Symbol] {
			// If the quote token isn't one of our common ones, skip for simplicity for now.
            // You could add logic here to handle pairs like XXX/YYY where neither is SOL/USDC.
			continue
		}

		// Apply Filters
		if pair.Liquidity.Usd < minLiquidityUSD {
			// log.Printf("DEBUG: Skip %s/%s - Low Liquidity: $%.2f", pair.BaseToken.Symbol, pair.QuoteToken.Symbol, pair.Liquidity.Usd)
			continue
		}
		if pair.Volume.M5 < minVolume5mUSD {
			// log.Printf("DEBUG: Skip %s/%s - Low 5m Volume: $%.2f", pair.BaseToken.Symbol, pair.QuoteToken.Symbol, pair.Volume.M5)
			continue
		}

		// Add to our list
		momentumCandidates = append(momentumCandidates, TokenMomentumInfo{
			PairAddress:     pair.PairAddress,
			BaseTokenSymbol: pair.BaseToken.Symbol,
			BaseTokenAddr:   pair.BaseToken.Address,
            QuoteTokenSymbol: pair.QuoteToken.Symbol,
			PriceChangeM5:   pair.PriceChange.M5,
			VolumeM5:        pair.Volume.M5,
			LiquidityUSD:    pair.Liquidity.Usd,
			PriceUSD:        pair.PriceUsd, // Keep as string, might be null/empty
			PairURL:         pair.URL,
		})
	}

	log.Printf("📊 Found %d candidate pairs after filtering.", len(momentumCandidates))

	if len(momentumCandidates) == 0 {
		log.Println("🤷 No pairs met the filtering criteria.")
		return
	}

	// 3. Sort candidates by 5-minute price change (descending)
	sort.Slice(momentumCandidates, func(i, j int) bool {
		// Handle NaN or Inf if necessary, though DexScreener data is usually clean
		return momentumCandidates[i].PriceChangeM5 > momentumCandidates[j].PriceChangeM5
	})

	// 4. Print the top N movers
	log.Printf("📈 Top %d Movers (5min change, >$%.0f liquidity, >$%.0f 5m Vol):", topMoversCount, minLiquidityUSD, minVolume5mUSD)
	count := 0
	for _, token := range momentumCandidates {
		if count >= topMoversCount {
			break
		}
		log.Printf("%2d. %-10s/%-4s | Change: %+.2f%% | Vol(5m): $%-8.0f | Liq: $%-10.0f | Price: %s | Pair: %s",
			count+1,
			token.BaseTokenSymbol,
            token.QuoteTokenSymbol,
			token.PriceChangeM5,
			token.VolumeM5,
			token.LiquidityUSD,
			token.PriceUSD,
			token.PairAddress,
			// token.PairURL, // Optionally print the URL
		)
		count++
	}

	log.Println("--- Scan Cycle Complete ---")
}

func main() {
	log.SetOutput(os.Stdout) // Ensure logs go to standard out
	log.Println("🚀 Starting DexScreener Momentum Scanner...")

	// Run the scan immediately first time
	runScan()

	// Then run in a loop
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop() // Ensure ticker is stopped when main exits

	for range ticker.C { // Block until the next tick
		runScan()
	}
}