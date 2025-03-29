// collector.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool" // PostgreSQL driver
)

// --- Configuration ---
const (
	// Database connection string (use environment variables in production!)
	dbConnectionString = "postgres://user:password@host:port/database_name?sslmode=disable"
	pollInterval       = 30 * time.Second // Adjust based on rate limits and needs
	// DexScreener API (Consider using specific pairs endpoint if list is fixed)
	dexScreenerAPIEndpoint = "https://api.dexscreener.com/latest/dex/search?q=SOL%20-meme%20-shitcoin" // Example: Search SOL pairs, try filtering noise
    // OR Use specific pairs endpoint (replace with actual addresses)
	// dexScreenerAPIEndpoint = "https://api.dexscreener.com/latest/dex/pairs/solana/PAIR_ADDR1,PAIR_ADDR2,PAIR_ADDR3"
    apiTimeout           = 15 * time.Second // Timeout for API requests
)

// --- Structs ---

// Simplified struct for database insertion
type PairSnapshotData struct {
	Timestamp        time.Time
	PairAddress      string
	BaseTokenAddress string
	BaseTokenSymbol  string
	QuoteTokenAddress string
	QuoteTokenSymbol string
	PriceNative      float64
	PriceUsd         float64
	LiquidityUsd     float64
	VolumeM5         float64
	VolumeH1         float64
	VolumeH6         float64
	VolumeH24        float64
	PriceChangeM5    float64
	PriceChangeH1    float64
	PriceChangeH6    float64
	PriceChangeH24   float64
	TxnsM5Buys       int
	TxnsM5Sells      int
	TxnsH1Buys       int
	TxnsH1Sells      int
	PairCreatedAt    time.Time
}

// DexScreener structs (simplified, add more fields if needed from Pair struct above)
type DexScreenerResponse struct {
	Pairs []Pair `json:"pairs"`
}
type Pair struct {
	ChainID     string    `json:"chainId"`
	PairAddress string    `json:"pairAddress"`
	BaseToken   Token     `json:"baseToken"`
	QuoteToken  Token     `json:"quoteToken"`
	PriceNative string    `json:"priceNative"`
	PriceUsd    string    `json:"priceUsd"`
	Txns        Transactions `json:"txns"`
	Volume      Volume    `json:"volume"`
	PriceChange PriceChange `json:"priceChange"`
	Liquidity   Liquidity `json:"liquidity"`
    PairCreatedAt int64   `json:"pairCreatedAt"`
}
type Token struct { Address string `json:"address"`; Symbol string `json:"symbol"` }
type Transactions struct { M5 BuysSells `json:"m5"`; H1 BuysSells `json:"h1"`}
type BuysSells struct { Buys int `json:"buys"`; Sells int `json:"sells"`}
type Volume struct { M5 float64 `json:"m5"`; H1 float64 `json:"h1"`; H6 float64 `json:"h6"`; H24 float64 `json:"h24"`}
type PriceChange struct { M5 float64 `json:"m5"`; H1 float64 `json:"h1"`; H6 float64 `json:"h6"`; H24 float64 `json:"h24"`}
type Liquidity struct { Usd float64 `json:"usd"` }

// --- Global DB Pool ---
var dbPool *pgxpool.Pool

// --- Helper Functions ---
func parseFloat(val string) float64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0 // Or NaN, depending on how you want to handle parse errors
	}
	return f
}

// --- API Fetching ---
func fetchDexScreenerData() ([]Pair, error) {
	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Get(dexScreenerAPIEndpoint)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		log.Println("⚠️ WARN: Hit Rate Limit (HTTP 429). Consider increasing poll interval.")
		// Optionally: return specific error or sleep before retry
        // time.Sleep(1 * time.Minute) // Example backoff
		return nil, fmt.Errorf("rate limited (429)")
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("non-OK HTTP status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}
	if len(bodyBytes) == 0 {
		log.Println("ℹ️ Received empty body from API.")
		return []Pair{}, nil
	}

	var apiResponse DexScreenerResponse
	if err := json.Unmarshal(bodyBytes, &apiResponse); err != nil {
		return nil, fmt.Errorf("error decoding DexScreener JSON: %w. Body segment: %s", err, string(bodyBytes[:min(len(bodyBytes), 200)]))
	}
	if apiResponse.Pairs == nil {
		log.Println("ℹ️ API response had null 'pairs' array.")
		return []Pair{}, nil
	}

	// Filter only Solana pairs client-side if using a broad search endpoint
    solanaPairs := []Pair{}
    if strings.Contains(dexScreenerAPIEndpoint, "/search") { // Apply only if search was used
        for _, p := range apiResponse.Pairs {
            if p.ChainID == "solana" {
                solanaPairs = append(solanaPairs, p)
            }
        }
        return solanaPairs, nil
    }

	return apiResponse.Pairs, nil // Return all if specific pairs were requested
}

// --- Database Operations ---
func insertSnapshotBatch(ctx context.Context, snapshots []PairSnapshotData) error {
	if len(snapshots) == 0 {
		return nil
	}

	// Use pgx CopyFrom for efficient bulk inserts
	rows := make([][]interface{}, len(snapshots))
	for i, s := range snapshots {
		rows[i] = []interface{}{
			s.Timestamp, s.PairAddress,
			s.BaseTokenAddress, s.BaseTokenSymbol, s.QuoteTokenAddress, s.QuoteTokenSymbol,
			s.PriceNative, s.PriceUsd, s.LiquidityUsd,
			s.VolumeM5, s.VolumeH1, s.VolumeH6, s.VolumeH24,
			s.PriceChangeM5, s.PriceChangeH1, s.PriceChangeH6, s.PriceChangeH24,
			s.TxnsM5Buys, s.TxnsM5Sells, s.TxnsH1Buys, s.TxnsH1Sells,
            s.PairCreatedAt,
		}
	}

	// Define columns in the order they appear in your rows slice
	columnNames := []string{
		"timestamp", "pair_address",
		"base_token_address", "base_token_symbol", "quote_token_address", "quote_token_symbol",
		"price_native", "price_usd", "liquidity_usd",
		"volume_m5", "volume_h1", "volume_h6", "volume_h24",
		"price_change_m5", "price_change_h1", "price_change_h6", "price_change_h24",
		"txns_m5_buys", "txns_m5_sells", "txns_h1_buys", "txns_h1_sells",
        "pair_created_at",
	}

	copyCount, err := dbPool.CopyFrom(
		ctx,
		pgxpool.Identifier{"pair_snapshots"}, // Table name
		columnNames,
		pgxpool.CopyFromRows(rows),
	)

	if err != nil {
        // Log the detailed error
        log.Printf("❌ Error inserting batch into DB: %v", err)
		return fmt.Errorf("dbPool.CopyFrom failed: %w", err)
	}

	if int(copyCount) != len(snapshots) {
        // This indicates some rows might have failed, possibly due to constraints
        log.Printf("⚠️ WARN: Expected to insert %d rows, but CopyFrom returned %d", len(snapshots), copyCount)
		// Consider logging the failed rows if possible, or investigate constraint violations (like PRIMARY KEY)
	}

	return nil
}


// --- Main Polling Loop ---
func runCollector() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	log.Printf("Collector started. Polling every %v. Saving to DB.", pollInterval)

	for range ticker.C {
		pollStartTime := time.Now()
        log.Printf("Polling API at %s...", pollStartTime.Format(time.RFC3339))

		pairs, err := fetchDexScreenerData()
		if err != nil {
			log.Printf("⚠️ Error fetching API data: %v. Skipping this cycle.", err)
			continue
		}
        if len(pairs) == 0 {
            log.Println("ℹ️ No pairs returned from API this cycle.")
            continue
        }

		log.Printf("ℹ️ Fetched data for %d pairs.", len(pairs))
		now := time.Now().UTC() // Use UTC for consistency

		var snapshots []PairSnapshotData
		for _, p := range pairs {
			// Basic validation
			if p.PairAddress == "" || p.BaseToken.Address == "" || p.QuoteToken.Address == "" {
                log.Printf("⚠️ Skipping pair due to missing address: %+v", p)
                continue
            }
            // Add more validation as needed (e.g., non-negative liquidity/volume)

			snapshots = append(snapshots, PairSnapshotData{
				Timestamp:        now,
				PairAddress:      p.PairAddress,
				BaseTokenAddress: p.BaseToken.Address,
				BaseTokenSymbol:  p.BaseToken.Symbol,
				QuoteTokenAddress: p.QuoteToken.Address,
				QuoteTokenSymbol: p.QuoteToken.Symbol,
				PriceNative:      parseFloat(p.PriceNative),
				PriceUsd:         parseFloat(p.PriceUsd),
				LiquidityUsd:     p.Liquidity.Usd,
				VolumeM5:         p.Volume.M5,
				VolumeH1:         p.Volume.H1,
                VolumeH6:         p.Volume.H6,
                VolumeH24:        p.Volume.H24,
				PriceChangeM5:    p.PriceChange.M5,
				PriceChangeH1:    p.PriceChange.H1,
                PriceChangeH6:    p.PriceChange.H6,
                PriceChangeH24:   p.PriceChange.H24,
				TxnsM5Buys:       p.Txns.M5.Buys,
				TxnsM5Sells:      p.Txns.M5.Sells,
                TxnsH1Buys:       p.Txns.H1.Buys, // Store H1 txns too if schema allows
                TxnsH1Sells:      p.Txns.H1.Sells,
                PairCreatedAt:    time.Unix(p.PairCreatedAt/1000, 0), // Convert ms to time.Time
			})
		}

		// Insert batch into database
        dbCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // DB operation timeout
		err = insertSnapshotBatch(dbCtx, snapshots)
        cancel() // Release context resources

		if err != nil {
			log.Printf("❌ Failed to insert batch: %v", err)
            // Consider retry logic or dead-letter queue here
		} else {
            log.Printf("✅ Inserted %d snapshots into DB. Cycle duration: %v", len(snapshots), time.Since(pollStartTime))
        }
	}
}

// --- Main Function ---
func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	var err error
	// Initialize database connection pool
	dbPool, err = pgxpool.New(context.Background(), dbConnectionString)
	if err != nil {
		log.Fatalf("❌ Unable to connect to database: %v\n", err)
	}
	defer dbPool.Close() // Ensure pool is closed on exit

	// Test DB connection
	err = dbPool.Ping(context.Background())
	if err != nil {
		log.Fatalf("❌ Unable to ping database: %v\n", err)
	}
	log.Println("✅ Database connection established.")

	// Start the collector loop
	runCollector()
}

// Helper for min function used in logging JSON parse errors
func min(a, b int) int {
    if a < b { return a }
    return b
}