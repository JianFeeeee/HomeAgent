package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func randomSecret(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	dataDir := flag.String("data", "", "data directory")
	webuiUsername := flag.String("username", "admin", "webui username")
	webuiPassword := flag.String("password", "", "webui password (auto-generated if empty)")
	webuiApiKey := flag.String("apikey", "", "api key (auto-generated if empty)")
	flag.Parse()

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "-data is required")
		os.Exit(1)
	}

	os.MkdirAll(*dataDir, 0755)

	dbPath := *dataDir + "/config.db"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	db.Exec("PRAGMA journal_mode=WAL")

	db.Exec(`CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, "webui.listen_addr", ":8080")

	pw := *webuiPassword
	if pw == "" {
		pw = randomSecret(12)
	}
	apiKey := *webuiApiKey
	if apiKey == "" {
		apiKey = randomSecret(16)
	}

	pt := "config_webui"
	db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (key TEXT PRIMARY KEY, value TEXT NOT NULL)`, pt))
	ws := fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, pt)
	db.Exec(ws, "api_key", apiKey)
	db.Exec(ws, "username", *webuiUsername)
	db.Exec(ws, "password", pw)
	db.Exec(ws, "session_ttl_hours", "24")

	fmt.Printf("API_KEY=%s\n", apiKey)
	fmt.Printf("WEBUI_USERNAME=%s\n", *webuiUsername)
	fmt.Printf("WEBUI_PASSWORD=%s\n", pw)
}
