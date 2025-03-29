// paperstrat.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math" // For Max/Min in normalization
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Constants ---
const (
	dexScreenerSearchAPI = "https://api.dexscreener.com/latest/dex/search"
	solanaChainID        = "solana"
	refreshInterval      = 30 * time.Second // Poll DexScreener every 30 seconds
	tradeSizeSOL         = 1.0              // Fixed SOL amount per trade
	simulatedFeePercent  = 0.003          // 0.3% Fee per side (0.6% round trip approx) - Jupiter is ~0.1-0.2% but add slippage allowance

	// File Names
	tradesLogFile = "trades.json"
	walletLogFile = "wallet_log.json"

	// Filtering Thresholds
	minLiquidityUSD = 2000.0            // Increase liquidity requirement
	minVolume5mUSD  = 500.0             // Min 5m volume in USD
	minPairAgeHours = 1.0               // Pair must be at least 1 hour old

	// Entry Scoring Weights (Tune These!)
	wM5Change        = 0.30 // 30% weight for 5m price change
	wH1Change        = 0.15 // 15% weight for 1h price change
	wM5Volume        = 0.20 // 20% weight for 5m volume (USD)
	wM5BuySellRatio  = 0.25 // 25% weight for 5m Buy/Sell Txn ratio
	wLiquidity       = 0.10 // 10% weight for current Liquidity (USD)
	minScoreToEnter  = 0.65 // Minimum normalized score (0-1) required to enter a trade

	// Exit Strategy Thresholds
	takeProfitThreshold     = 1.05  // 5% Take Profit
	trailingStopLossPercent = 0.03  // 3% Trailing Stop Loss
	momentumFadeExitM5      = 0.001 // Exit if 5m change drops below 0.1%
	liquidityDropPercent    = 0.30  // Exit if liquidity drops by 30% from entry

	// Display Constants
	topScorersCount = 10 // Display top 10 scored pairs
)

// --- Structs ---

// DexScreener structs (same as before)
type DexScreenerResponse struct { SchemaVersion string `json:"schemaVersion"`; Pairs []Pair `json:"pairs"` }
type Pair struct { ChainID string `json:"chainId"`; DexID string `json:"dexId"`; URL string `json:"url"`; PairAddress string `json:"pairAddress"`; BaseToken Token `json:"baseToken"`; QuoteToken Token `json:"quoteToken"`; PriceNative string `json:"priceNative"`; PriceUsd string `json:"priceUsd"`; Txns Transactions `json:"txns"`; Volume Volume `json:"volume"`; PriceChange PriceChange `json:"priceChange"`; Liquidity Liquidity `json:"liquidity"`; Fdv float64 `json:"fdv"`; PairCreatedAt int64 `json:"pairCreatedAt"`}
type Token struct { Address string `json:"address"`; Name string `json:"name"`; Symbol string `json:"symbol"` }
type Transactions struct { M5 BuysSells `json:"m5"`; H1 BuysSells `json:"h1"`; H6 BuysSells `json:"h6"`; H24 BuysSells `json:"h24"`}
type BuysSells struct { Buys int `json:"buys"`; Sells int `json:"sells"`}
type Volume struct { H24 float64 `json:"h24"`; H6 float64 `json:"h6"`; H1 float64 `json:"h1"`; M5 float64 `json:"m5"`}
type PriceChange struct { M5 float64 `json:"m5"`; H1 float64 `json:"h1"`; H6 float64 `json:"h6"`; H24 float64 `json:"h24"`}
type Liquidity struct { Usd float64 `json:"usd"`; Base float64 `json:"base"`; Quote float64 `json:"quote"`}


// Enhanced structure for processing and scoring
type TokenInfo struct {
	PairAddress      string
	BaseTokenSymbol  string
	BaseTokenAddr    string
	QuoteTokenSymbol string
	QuoteTokenAddr   string
	PairCreatedAt    time.Time
	PriceNative      float64 // Parsed PriceNative
	PriceUSD         float64 // Parsed PriceUSD
	LiquidityUSD     float64 // From Liquidity.Usd
	PriceChangeM5    float64
	PriceChangeH1    float64
	VolumeM5         float64 // From Volume.m5
	M5BuySellRatio   float64 // Calculated: Buys / (Buys + Sells) or similar
	PairURL          string

	// Score components (normalized 0-1)
	NormM5Change      float64
	NormH1Change      float64
	NormM5Volume      float64
	NormM5BuySellRatio float64
	NormLiquidity     float64
	Score             float64 // Final weighted score
}

// Paper Trading State
type PaperWallet struct {
	SOLBalance      float64 `json:"solBalance"`
	InitialSOL      float64 `json:"-"` // Not logged every time
	TradesMade      int     `json:"tradesMade"`
	ProfitableTrades int     `json:"profitableTrades"`
	TotalFeesPaid   float64 `json:"totalFeesPaid"`
}

type CurrentHolding struct {
	Active           bool      `json:"active"`
	BaseTokenSymbol  string    `json:"baseTokenSymbol,omitempty"`
	BaseTokenAddr    string    `json:"baseTokenAddr,omitempty"`
	QuoteTokenSymbol string    `json:"quoteTokenSymbol,omitempty"`
	QuoteTokenAddr   string    `json:"quoteTokenAddr,omitempty"`
	PairAddress      string    `json:"pairAddress,omitempty"`
	AmountToken      float64   `json:"amountToken,omitempty"`
	EntryPriceNative float64   `json:"entryPriceNative,omitempty"`
	EntryTime        time.Time `json:"entryTime,omitempty"`
	EntryLiquidityUSD float64   `json:"entryLiquidityUSD,omitempty"` // Track initial liquidity
	PeakPriceNative  float64   `json:"peakPriceNative,omitempty"`   // For trailing stop loss
}

// Structs for JSON Logging
type TradeLogEntry struct {
	Timestamp     time.Time `json:"timestamp"`
	Action        string    `json:"action"` // "BUY" or "SELL"
	Symbol        string    `json:"symbol"`
	PairAddress   string    `json:"pairAddress"`
	SOLAmount     float64   `json:"solAmount"`    // SOL spent (BUY) or received gross (SELL)
	TokenAmount   float64   `json:"tokenAmount"`  // Tokens bought or sold
	PriceNative   float64   `json:"priceNative"`  // Execution price in SOL
	FeeSOL        float64   `json:"feeSOL"`       // Estimated fee for this action
	ProfitLossSOL float64   `json:"profitLossSOL,omitempty"` // For SELL actions only (Net P/L for the trade)
	Reason        string    `json:"reason,omitempty"`      // Reason for SELL
}

type WalletLogEntry struct {
	Timestamp    time.Time     `json:"timestamp"`
	SOLBalance   float64     `json:"solBalance"`
	Holding      CurrentHolding `json:"holding"` // Embed holding status
	TradesMade   int         `json:"tradesMade"`
	FeesPaid     float64     `json:"feesPaid"`
}


// --- Global State ---
var wallet PaperWallet
var holding CurrentHolding

// --- Initialization ---
func initPaperTrading() {
	wallet = PaperWallet{
		SOLBalance:      10.0,
		InitialSOL:      10.0,
		TradesMade:      0,
		ProfitableTrades: 0,
		TotalFeesPaid:   0.0,
	}
	holding = CurrentHolding{Active: false}
	log.Printf("💰 Paper Trading Initialized: %.4f SOL", wallet.SOLBalance)
    // Log initial wallet state
    logWalletState()
}

// --- Helper Functions ---

func parseFloat(val string, defaultVal float64) float64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

func calculateBuySellRatio(buys, sells int) float64 {
	totalTxns := buys + sells
	if totalTxns == 0 {
		return 0.5 // Neutral if no transactions
	}
	return float64(buys) / float64(totalTxns)
}

func normalize(value, min, max float64) float64 {
	if max-min == 0 {
		return 0 // Avoid division by zero; return neutral or zero
	}
	return (value - min) / (max - min)
}

// Append JSON object to a file, one object per line
func appendJSONToFile(filename string, data interface{}) error {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filename, err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode JSON to %s: %w", filename, err)
	}
	return nil
}

// Log Trade Action (Console and JSON)
func logTradeAction(logEntry TradeLogEntry) {
	actionUpper := strings.ToUpper(logEntry.Action)
    pnlString := ""
    if actionUpper == "SELL" {
        pnlString = fmt.Sprintf(" | P/L: %.5f SOL", logEntry.ProfitLossSOL)
        if logEntry.Reason != "" {
             pnlString += " (" + logEntry.Reason + ")"
        }
    }

	log.Printf("📄 TRADE %s: %s [%.5f tokens @ %.8f SOL] SOL Amt: %.5f (Fee: %.6f)%s | Pair: %s",
		actionUpper,
		logEntry.Symbol,
        logEntry.TokenAmount,
		logEntry.PriceNative,
		logEntry.SOLAmount,
        logEntry.FeeSOL,
		pnlString,
        logEntry.PairAddress,
	)

    if err := appendJSONToFile(tradesLogFile, logEntry); err != nil {
		log.Printf("⚠️ Error logging trade to JSON file: %v", err)
	}
}

// Log Current Wallet State (Console Brief + JSON Detailed)
func logWalletState() {
     log.Printf("🏦 Wallet State: %.4f SOL | Trades: %d (%.1f%% Profitable) | Fees: %.6f SOL | Holding: %t",
        wallet.SOLBalance,
        wallet.TradesMade,
        profitabilityPercent(),
        wallet.TotalFeesPaid,
        holding.Active,
    )

	entry := WalletLogEntry{
		Timestamp:  time.Now(),
		SOLBalance: wallet.SOLBalance,
		Holding:    holding, // Log current holding details
        TradesMade: wallet.TradesMade,
        FeesPaid:   wallet.TotalFeesPaid,
	}
	if err := appendJSONToFile(walletLogFile, entry); err != nil {
		log.Printf("⚠️ Error logging wallet state to JSON file: %v", err)
	}
}

func profitabilityPercent() float64 {
    if wallet.TradesMade == 0 {
        return 0.0
    }
    return (float64(wallet.ProfitableTrades) / float64(wallet.TradesMade)) * 100.0
}


// --- API Fetching ---
func fetchDexScreenerPairs(query string) ([]Pair, error) {
	url := fmt.Sprintf("%s?q=%s", dexScreenerSearchAPI, query)
	// log.Printf("⏳ Fetching DexScreener data: %s", url) // Less verbose

	client := http.Client{Timeout: 10 * time.Second} // Add timeout
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error fetching from DexScreener: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed DexScreener fetch: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading DexScreener response body: %w", err)
	}
	if len(bodyBytes) == 0 { return []Pair{}, nil }

	var apiResponse DexScreenerResponse
	if err := json.Unmarshal(bodyBytes, &apiResponse); err != nil {
		return nil, fmt.Errorf("error decoding DexScreener JSON: %w", err)
	}
	if apiResponse.Pairs == nil { return []Pair{}, nil }

    // Basic filter for Solana before returning (optional optimization)
    solanaPairs := []Pair{}
    for _, p := range apiResponse.Pairs {
        if p.ChainID == solanaChainID {
            solanaPairs = append(solanaPairs, p)
        }
    }
	// log.Printf("ℹ️ Fetched %d pairs, %d on Solana.", len(apiResponse.Pairs), len(solanaPairs))
	return solanaPairs, nil
}


// --- Scoring Logic ---
func calculateScores(candidates []TokenInfo) []TokenInfo {
	if len(candidates) < 2 { // Need at least 2 points to normalize meaningfully
        for i := range candidates {
            candidates[i].Score = 0 // Assign default score if only one or zero candidates
        }
		return candidates
	}

	// Find min/max for each component for normalization
	minM5, maxM5 := candidates[0].PriceChangeM5, candidates[0].PriceChangeM5
	minH1, maxH1 := candidates[0].PriceChangeH1, candidates[0].PriceChangeH1
	minVol, maxVol := candidates[0].VolumeM5, candidates[0].VolumeM5
	minRatio, maxRatio := candidates[0].M5BuySellRatio, candidates[0].M5BuySellRatio
	minLiq, maxLiq := candidates[0].LiquidityUSD, candidates[0].LiquidityUSD

	for _, c := range candidates[1:] {
		minM5 = math.Min(minM5, c.PriceChangeM5)
		maxM5 = math.Max(maxM5, c.PriceChangeM5)
		minH1 = math.Min(minH1, c.PriceChangeH1)
		maxH1 = math.Max(maxH1, c.PriceChangeH1)
		minVol = math.Min(minVol, c.VolumeM5)
		maxVol = math.Max(maxVol, c.VolumeM5)
		minRatio = math.Min(minRatio, c.M5BuySellRatio)
		maxRatio = math.Max(maxRatio, c.M5BuySellRatio)
		minLiq = math.Min(minLiq, c.LiquidityUSD)
		maxLiq = math.Max(maxLiq, c.LiquidityUSD)
	}

	// Calculate normalized values and final score for each candidate
	scoredCandidates := make([]TokenInfo, len(candidates))
	for i, c := range candidates {
		c.NormM5Change = normalize(c.PriceChangeM5, minM5, maxM5)
		c.NormH1Change = normalize(c.PriceChangeH1, minH1, maxH1)
		c.NormM5Volume = normalize(c.VolumeM5, minVol, maxVol)
		c.NormM5BuySellRatio = normalize(c.M5BuySellRatio, minRatio, maxRatio)
		c.NormLiquidity = normalize(c.LiquidityUSD, minLiq, maxLiq)

		c.Score = (c.NormM5Change * wM5Change) +
			(c.NormH1Change * wH1Change) +
			(c.NormM5Volume * wM5Volume) +
			(c.NormM5BuySellRatio * wM5BuySellRatio) +
			(c.NormLiquidity * wLiquidity)

		scoredCandidates[i] = c // Store the updated struct
	}

	return scoredCandidates
}

// --- Main Scan and Trade Logic ---
func runScan() {
	// log.Println("--- Scan Cycle Start ---") // Less verbose

	// 1. Fetch Data
	pairs, err := fetchDexScreenerPairs("SOL") // Query likely less important now with strict filtering
	if err != nil {
		log.Printf("⚠️ Error fetching pairs: %v. Skipping cycle.", err)
		return
	}

	// 2. Filter & Process Pairs
	var candidates []TokenInfo
	currentPairData := make(map[string]TokenInfo) // Map PairAddress -> Info for quick lookup
	minTime := time.Now().Add(-time.Duration(minPairAgeHours * float64(time.Hour)))

	for _, pair := range pairs {
        // Primary Filters
		if pair.QuoteToken.Symbol != "SOL" { continue } // Must be vs SOL
		if pair.Liquidity.Usd < minLiquidityUSD { continue }
		if pair.Volume.M5 < minVolume5mUSD { continue }
        createdAt := time.Unix(pair.PairCreatedAt/1000, 0) // DexScreener uses ms timestamps
        if createdAt.After(minTime) { continue } // Check age

        priceNative := parseFloat(pair.PriceNative, -1.0)
        if priceNative <= 0 { continue } // Invalid price

		// Extract data into our TokenInfo struct
		info := TokenInfo{
			PairAddress:      pair.PairAddress,
			BaseTokenSymbol:  pair.BaseToken.Symbol,
			BaseTokenAddr:    pair.BaseToken.Address,
			QuoteTokenSymbol: pair.QuoteToken.Symbol, // SOL
			QuoteTokenAddr:   pair.QuoteToken.Address,
            PairCreatedAt:    createdAt,
			PriceNative:      priceNative,
            PriceUSD:         parseFloat(pair.PriceUsd, 0.0),
			LiquidityUSD:     pair.Liquidity.Usd,
			PriceChangeM5:    pair.PriceChange.M5,
			PriceChangeH1:    pair.PriceChange.H1,
			VolumeM5:         pair.Volume.M5,
            M5BuySellRatio:   calculateBuySellRatio(pair.Txns.M5.Buys, pair.Txns.M5.Sells),
			PairURL:          pair.URL,
		}
		candidates = append(candidates, info)
		currentPairData[pair.PairAddress] = info
	}

    // log.Printf("ℹ️ Found %d pairs meeting initial filters.", len(candidates))

	// 3. Score Candidates
	scoredCandidates := calculateScores(candidates)

	// 4. Exit Logic
	var walletUpdated bool = false
	if holding.Active {
		currentData, found := currentPairData[holding.PairAddress]
        sellReason := ""
        sellPrice := 0.0

		if !found {
			log.Printf("⚠️ Held token %s (%s) PAIR DATA NOT FOUND in current scan. Holding position.", holding.BaseTokenSymbol, holding.PairAddress)
            // Policy decision: Maybe implement forceful exit if data missing for X cycles?
		} else {
			// Update peak price for trailing SL
			holding.PeakPriceNative = math.Max(holding.PeakPriceNative, currentData.PriceNative)
            currentPrice := currentData.PriceNative
            sellPrice = currentPrice // Assume selling at current market price

			// Check exit conditions in priority order
            liquidityThreshold := holding.EntryLiquidityUSD * (1.0 - liquidityDropPercent)
            trailingStopPrice := holding.PeakPriceNative * (1.0 - trailingStopLossPercent)
            takeProfitPrice := holding.EntryPriceNative * takeProfitThreshold

            if currentData.LiquidityUSD < liquidityThreshold {
                sellReason = fmt.Sprintf("Liquidity Drop (< %.0f USD)", liquidityThreshold)
            } else if currentPrice <= trailingStopPrice {
                sellReason = fmt.Sprintf("Trailing Stop Loss (< %.8f SOL)", trailingStopPrice)
            } else if currentPrice >= takeProfitPrice {
                sellReason = "Take Profit"
            } else if currentData.PriceChangeM5 < momentumFadeExitM5 && time.Since(holding.EntryTime) > 5*time.Minute { // Add time buffer to mom fade
                 sellReason = fmt.Sprintf("Momentum Fade (m5 < %.3f%%)", momentumFadeExitM5*100)
            }
             // Add time-based stop if desired
             // else if time.Since(holding.EntryTime) > maxHoldDuration { sellReason = "Time Stop" }
        }


        // Execute Sell if reason found
        if sellReason != "" {
            log.Printf("📈 SELL Signal for %s (%s)", holding.BaseTokenSymbol, sellReason)

            // Calculate sell proceeds and fee
            solReceivedGross := holding.AmountToken * sellPrice
            feeAmount := solReceivedGross * simulatedFeePercent
            solReceivedNet := solReceivedGross - feeAmount

            // Calculate P/L for this specific trade
            // solSpentOnBuy := holding.EntryPriceNative * holding.AmountToken // Approx initial SOL cost (ignores buy fee here for simplicity of P/L calc)
            initialBuyCostBasis := tradeSizeSOL // More accurate basis is the fixed trade size
            profitLoss := solReceivedNet - initialBuyCostBasis

            // Update wallet
            wallet.SOLBalance += solReceivedNet
            wallet.TotalFeesPaid += feeAmount // Add fee from this side of trade
            wallet.TradesMade++
            if profitLoss > 0 {
                wallet.ProfitableTrades++
            }

            // Log trade
            tradeLog := TradeLogEntry{
                Timestamp:     time.Now(),
                Action:        "SELL",
                Symbol:        holding.BaseTokenSymbol,
                PairAddress:   holding.PairAddress,
                SOLAmount:     solReceivedGross,
                TokenAmount:   holding.AmountToken,
                PriceNative:   sellPrice,
                FeeSOL:        feeAmount,
                ProfitLossSOL: profitLoss,
                Reason:        sellReason,
            }
            logTradeAction(tradeLog)
            holding.Active = false // Clear holding state
            walletUpdated = true
        } else if found {
             // Log holding status if no sell triggered but data was found
             log.Printf(" HOLDING: %s (%.5f) @ Entry: %.8f | Cur: %.8f | Peak: %.8f | TSL: %.8f | Liq: %.0f",
                    holding.BaseTokenSymbol, holding.AmountToken, holding.EntryPriceNative,
                    currentData.PriceNative, holding.PeakPriceNative, holding.PeakPriceNative*(1.0-trailingStopLossPercent), currentData.LiquidityUSD)
        }

	}


	// 5. Entry Logic (only if not holding)
	if !holding.Active && len(scoredCandidates) > 0 {
		// Sort by score descending
		sort.Slice(scoredCandidates, func(i, j int) bool {
			return scoredCandidates[i].Score > scoredCandidates[j].Score
		})

        // Optionally print top scorers before deciding entry
        printTopScorers(scoredCandidates)


		// Evaluate top candidate for entry
		topCandidate := scoredCandidates[0]
		if topCandidate.Score >= minScoreToEnter && wallet.SOLBalance >= tradeSizeSOL {
			log.Printf("📉 BUY Signal for %s (Score: %.4f >= %.4f)", topCandidate.BaseTokenSymbol, topCandidate.Score, minScoreToEnter)

            // Calculate buy details and fee
            entryPrice := topCandidate.PriceNative
            tokenAmountToBuy := tradeSizeSOL / entryPrice // Ideal amount ignoring fee
            feeAmount := tradeSizeSOL * simulatedFeePercent // Fee on the SOL spent
            solToSpend := tradeSizeSOL + feeAmount // Need enough SOL for trade size + fee

            if wallet.SOLBalance < solToSpend {
                log.Printf("ℹ️ Insufficient SOL (%.5f) for trade + fee (%.5f). Skipping BUY.", wallet.SOLBalance, solToSpend)
            } else {
                // Update wallet
                wallet.SOLBalance -= solToSpend
                wallet.TotalFeesPaid += feeAmount

                // Set holding state
                holding = CurrentHolding{
                    Active:           true,
                    BaseTokenSymbol:  topCandidate.BaseTokenSymbol,
                    BaseTokenAddr:    topCandidate.BaseTokenAddr,
                    QuoteTokenSymbol: topCandidate.QuoteTokenSymbol, // SOL
                    QuoteTokenAddr:   topCandidate.QuoteTokenAddr,
                    PairAddress:      topCandidate.PairAddress,
                    AmountToken:      tokenAmountToBuy, // Store amount bought *before* fee deduction from SOL
                    EntryPriceNative: entryPrice,
                    EntryTime:        time.Now(),
                    PeakPriceNative:  entryPrice, // Initialize peak price to entry price
                    EntryLiquidityUSD: topCandidate.LiquidityUSD, // Store liquidity at entry
                }

                // Log trade
                tradeLog := TradeLogEntry{
                    Timestamp:     time.Now(),
                    Action:        "BUY",
                    Symbol:        holding.BaseTokenSymbol,
                    PairAddress:   holding.PairAddress,
                    SOLAmount:     tradeSizeSOL, // Log the intended trade size, fee tracked separately
                    TokenAmount:   holding.AmountToken,
                    PriceNative:   holding.EntryPriceNative,
                    FeeSOL:        feeAmount,
                }
                logTradeAction(tradeLog)
                walletUpdated = true
            }
		} else {
            log.Printf("ℹ️ Top candidate %s Score %.4f < %.4f OR Insufficient SOL. No BUY.", topCandidate.BaseTokenSymbol, topCandidate.Score, minScoreToEnter)
        }

	} else if len(scoredCandidates) == 0 && !holding.Active{
        log.Println("🤷 No suitable candidates found after filtering and scoring.")
    }


    // 6. Log Wallet State if Updated or Periodically (e.g., every 10th cycle)
    // Add a counter if periodic logging is desired
    if walletUpdated {
	    logWalletState() // Log wallet immediately after a trade
    }

	// log.Println("--- Scan Cycle End ---") // Less verbose
}


// Helper to print top N scored tokens
func printTopScorers(scoredCandidates []TokenInfo) {
     log.Printf("--- Top %d Scored Tokens ---", topScorersCount)
     count := 0
     for _, c := range scoredCandidates { // Assumes already sorted
         if count >= topScorersCount { break }
         log.Printf("%2d. %-10s | Score: %.4f [m5:%.2f(%.2f) h1:%.2f(%.2f) vol:%.0f(%.2f) b/s:%.2f(%.2f) liq:%.0f(%.2f)] | Pair: %s",
             count+1,
             c.BaseTokenSymbol,
             c.Score,
             c.PriceChangeM5, c.NormM5Change,       // Raw (Norm)
             c.PriceChangeH1, c.NormH1Change,
             c.VolumeM5, c.NormM5Volume,
             c.M5BuySellRatio, c.NormM5BuySellRatio,
             c.LiquidityUSD, c.NormLiquidity,
             c.PairAddress,
         )
         count++
     }
     log.Println("--------------------------")
}

// --- Main Execution Loop ---
func main() {
	log.SetOutput(os.Stdout) // Ensure logs go to standard out
    log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds) // Add microsecond precision
	log.Println("🚀 Starting Advanced Paper Trading Bot...")
	initPaperTrading()

	// Run first scan immediately
	runScan()

	// Start ticker loop
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	for range ticker.C {
		runScan()
	}
    // Add signal handling for graceful shutdown here if needed
}