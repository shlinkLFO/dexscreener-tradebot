// pumpfun_sniperbot.go
package main

import (
	"sort"
//	"io"
//	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
//	"github.com/gagliardetto/solana-go/rpc"
)

// helper to parse float values safely
func entryFloat(val interface{}) float64 {
	if v, ok := val.(float64); ok {
		return v
	}
	return 0
}

// TokenListing represents a token listed on Pump.fun
// Includes historical prices for momentum tracking
type TokenListing struct {
	Name       string  `json:"name"`
	Address    string  `json:"address"`
	Liquidity  float64 `json:"liquidity"`
	Price      float64 `json:"price"`
	CreatedAt  int64   `json:"created_at"`
	PrevPrice  float64 `json:"-"`
	Momentum   float64 `json:"-"`
}

// TradeLog holds simulated trade data
type TradeLog struct {
	Timestamp     string  `json:"timestamp"`
	TokenName     string  `json:"token_name"`
	TokenAddress  string  `json:"token_address"`
	AmountSOL     float64 `json:"amount_sol"`
	ExpectedOut   float64 `json:"expected_amount"`
	Slippage      float64 `json:"slippage"`
	FeeEstimate   float64 `json:"fee_estimate_sol"`
}

// WalletLog holds balance snapshot data
type WalletLog struct {
	Timestamp string  `json:"timestamp"`
	SOL       float64 `json:"sol_balance"`
	Token     float64 `json:"token_estimate"`
}

// Global price history cache for momentum tracking
var priceCache = map[string]float64{}

func fetchListings() ([]TokenListing, error) {
	url := "https://cache.jup.ag/tokens"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tokens []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, err
	}

	var listings []TokenListing
	for _, token := range tokens {
		address := fmt.Sprintf("%v", token["address"])
		name := fmt.Sprintf("%v", token["name"])
		if address == "" || name == "" || address == "So11111111111111111111111111111111111111112" {
			continue
		}

		price := 0.0
		quoteUrl := fmt.Sprintf("https://quote-api.jup.ag/v6/quote?inputMint=So11111111111111111111111111111111111111112&outputMint=%s&amount=10000000", address)
		res, err := http.Get(quoteUrl)
		if err != nil {
			continue
		}
		var result map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			res.Body.Close()
			continue
		}
		res.Body.Close()

		outStr, ok := result["outAmount"].(string)
		if !ok || outStr == "" {
			continue
		}
		fmt.Sscanf(outStr, "%f", &price)
		price = price / 1e9

		prev := priceCache[address]
		priceCache[address] = price
		momentum := 0.0
		if prev > 0 {
			momentum = (price - prev) / prev
		}

		listings = append(listings, TokenListing{
			Name:      name,
			Address:   address,
			Liquidity: 0,
			Price:     price,
			CreatedAt: time.Now().Unix(),
			PrevPrice: prev,
			Momentum:  momentum,
		})
	}
	return listings, nil
}

func GenerateSolanaWallet() (solana.PrivateKey, error) {
	key := solana.NewWallet().PrivateKey
	err := os.WriteFile("wallet.json", []byte(fmt.Sprintf("\"%s\"", key.String())), 0600)
	if err != nil {
		return nil, err
	}
	log.Printf("🔐 Wallet generated and saved to wallet.json")
	log.Printf("🔑 Public Key: %s", key.PublicKey().String())
	return key, nil
}

func LoadSolanaWallet() (solana.PrivateKey, error) {
	data, err := os.ReadFile("wallet.json")
	if err != nil {
		return nil, err
	}
	keyStr := strings.Trim(string(data), "\"\n")
	key, err := solana.PrivateKeyFromBase58(keyStr)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func logTrade(trade TradeLog) {
	f, _ := os.OpenFile("trades.json", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	json.NewEncoder(f).Encode(trade)
}

func logWallet(wallet WalletLog) {
	f, _ := os.OpenFile("wallet_balances.json", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	json.NewEncoder(f).Encode(wallet)
}

func main() {
	log.Println("🚀 Starting Pump.fun SniperBot...")
	key, err := LoadSolanaWallet()
	if err != nil {
		log.Println("⚠️ Wallet not found, generating new one...")
		key, err = GenerateSolanaWallet()
		if err != nil {
			log.Fatal("❌ Failed to generate wallet:", err)
		}
	}
	log.Printf("🔑 Loaded Wallet Public Key: %s", key.PublicKey().String())

	listings, err := fetchListings()
	if err != nil || len(listings) == 0 {

	// Sort by momentum descending
	sort.Slice(listings, func(i, j int) bool {
		return listings[i].Momentum > listings[j].Momentum
	})

	log.Println("📊 Top 10 Momentum Tokens:")
	for i, token := range listings {
		if i >= 10 {
			break
		}
		log.Printf("%2d. %s | %.6f SOL | %+.2f%% momentum | %s", i+1, token.Name, token.Price, token.Momentum*100, token.Address)
	}
		log.Fatal("❌ Could not fetch live tokens")
	}

	// Find top trending token based on momentum and liquidity
	var pick TokenListing
	for _, token := range listings {
		if token.Liquidity > 10 && token.Momentum > 0.1 { // >10% growth
			pick = token
			break
		}
	}
	if pick.Address == "" {
		log.Println("⚠️ No strong momentum token found")
		return
	}

	inputMint := "So11111111111111111111111111111111111111112"
	amountLamports := 500_000_000
	quoteUrl := fmt.Sprintf("https://quote-api.jup.ag/v6/quote?inputMint=%s&outputMint=%s&amount=%d&slippage=1", inputMint, pick.Address, amountLamports)
	resp, err := http.Get(quoteUrl)
	if err != nil {
		log.Fatalf("❌ Failed to get Jupiter quote: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Fatalf("❌ Jupiter decode error: %v", err)
	}
	outStr, ok := result["outAmount"].(string)
	if !ok {
		log.Fatalf("❌ Missing 'outAmount' in Jupiter response")
	}
	var outAmount float64
	fmt.Sscanf(outStr, "%f", &outAmount)
	outAmount = outAmount / 1e9

	slippage := 0.01
	if slippageStr, ok := result["otherAmountThreshold"].(string); ok {
		var threshold float64
		fmt.Sscanf(slippageStr, "%f", &threshold)
		threshold = threshold / 1e9
		if threshold > 0 {
			slippage = (threshold - outAmount) / threshold
		}
	}
	timestamp := time.Now().Format(time.RFC3339)

	logTrade(TradeLog{
		Timestamp:     timestamp,
		TokenName:     pick.Name,
		TokenAddress:  pick.Address,
		AmountSOL:     0.5,
		ExpectedOut:   outAmount,
		Slippage:      slippage,
		FeeEstimate:   0.0005,
	})

	logWallet(WalletLog{
		Timestamp: timestamp,
		SOL:       0.5,
		Token:     outAmount,
	})
}
