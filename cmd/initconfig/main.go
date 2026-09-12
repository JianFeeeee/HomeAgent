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

// must 让失败真正停下来。
//
// 这里曾经把所有 db.Exec 的返回值丢掉，配合 CGO_ENABLED=0 构建（go-sqlite3
// 退化成静态桩），得到的是一个**完全静默的空操作**：打印凭据、退出码 0、
// config.db 里一个字节都没写。调用方（安装脚本）无法区分成败，用户装完
// 照着 credentials.txt 登录必然失败。
func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: %v\n", err)
		os.Exit(1)
	}
}

// verify 回读刚写入的值。
//
// 只看 Exec 有没有报错不够：驱动被换掉（如上面的桩）、路径不对、写入被丢弃，
// 都可能返回 nil 而什么都没落下。这里把真实落盘的值读回来，与预期逐一比对，
// 不一致就非零退出——"初始化脚本说自己成功了"必须由数据库内容佐证。
func verify(db *sql.DB, table, key, want string) {
	var got string
	if err := db.QueryRow(fmt.Sprintf(`SELECT value FROM %s WHERE key = ?`, table), key).Scan(&got); err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: 回读 %s.%s 失败: %v\n", table, key, err)
		os.Exit(1)
	}
	if got != want {
		fmt.Fprintf(os.Stderr, "initconfig: %s.%s 与写入值不一致（读回 %q）\n", table, key, got)
		os.Exit(1)
	}
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
	must(err)
	defer db.Close()

	// 尽早验证数据库真的可用：sql.Open 是惰性的，不碰一次不会暴露驱动问题。
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: 打开数据库 %s 失败: %v\n", dbPath, err)
		os.Exit(1)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: 创建 config 表失败: %v\n", err)
		os.Exit(1)
	}
	const listenAddr = ":8080"
	if _, err := db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, "webui.listen_addr", listenAddr); err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: 写入 webui.listen_addr 失败: %v\n", err)
		os.Exit(1)
	}

	pw := *webuiPassword
	if pw == "" {
		pw = randomSecret(12)
	}
	apiKey := *webuiApiKey
	if apiKey == "" {
		apiKey = randomSecret(16)
	}

	const pt = "config_webui"
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (key TEXT PRIMARY KEY, value TEXT NOT NULL)`, pt)); err != nil {
		fmt.Fprintf(os.Stderr, "initconfig: 创建 %s 表失败: %v\n", pt, err)
		os.Exit(1)
	}
	ws := fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, pt)
	for _, kv := range [][2]string{
		{"api_key", apiKey},
		{"username", *webuiUsername},
		{"password", pw},
		{"session_ttl_hours", "24"},
	} {
		if _, err := db.Exec(ws, kv[0], kv[1]); err != nil {
			fmt.Fprintf(os.Stderr, "initconfig: 写入 %s.%s 失败: %v\n", pt, kv[0], err)
			os.Exit(1)
		}
	}

	verify(db, pt, "api_key", apiKey)
	verify(db, pt, "username", *webuiUsername)
	verify(db, pt, "password", pw)
	verify(db, "config", "webui.listen_addr", listenAddr)

	fmt.Printf("API_KEY=%s\n", apiKey)
	fmt.Printf("WEBUI_USERNAME=%s\n", *webuiUsername)
	fmt.Printf("WEBUI_PASSWORD=%s\n", pw)
}
