package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	socket := flag.String("socket", "/var/lib/homeagent/cli.sock", "unix socket path")
	remote := flag.String("remote", "", "remote webui URL (e.g. http://127.0.0.1:8080)")
	say := flag.String("say", "", "send a message and print response (one-shot, no TUI)")
	flag.Parse()

	if *say != "" {
		if *remote != "" {
			sayRemote(*remote, *say)
		} else {
			sayLocal(*socket, *say)
		}
		return
	}

	if *remote != "" {
		runRemote(*remote)
	} else {
		runLocal(*socket)
	}
}

// sayLocal sends one message via Unix socket and prints the response
func sayLocal(socketPath, message string) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		log.Fatalf("connect to %s: %v", socketPath, err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "%s\n", message)

	scanner := bufio.NewScanner(conn)
	scanner.Scan()
	if err := scanner.Err(); err != nil {
		log.Fatalf("read: %v", err)
	}

	var resp struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		fmt.Println(scanner.Text())
		return
	}
	switch resp.Type {
	case "response":
		fmt.Println(resp.Content)
	case "error":
		log.Fatalf("error: %s", resp.Error)
	default:
		fmt.Println(scanner.Text())
	}
}

// sayRemote sends one message via HTTP and prints the response
func sayRemote(baseURL, message string) {
	baseURL = strings.TrimRight(baseURL, "/")
	body := fmt.Sprintf(`{"message":%q}`, message)
	resp, err := http.Post(baseURL+"/api/v1/chat", "application/json", strings.NewReader(body))
	if err != nil {
		log.Fatalf("http post: %v", err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if content, ok := result["response"].(string); ok {
		fmt.Println(content)
	}
}

// runLocal starts an interactive TUI via Unix socket
func runLocal(socketPath string) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		log.Fatalf("connect to %s: %v", socketPath, err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var resp struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
				fmt.Println(scanner.Text())
				continue
			}
			switch resp.Type {
			case "response":
				fmt.Println(resp.Content)
			case "error":
				fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
			default:
				fmt.Println(scanner.Text())
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		fmt.Fprintf(conn, "%s\n", line)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
	}
}

// runRemote starts an interactive TUI via HTTP
func runRemote(baseURL string) {
	baseURL = strings.TrimRight(baseURL, "/")
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		body := fmt.Sprintf(`{"message":%q}`, line)
		resp, err := http.Post(baseURL+"/api/v1/chat", "application/json", strings.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			fmt.Print("> ")
			continue
		}
		var result map[string]interface{}
		if resp.StatusCode == http.StatusOK {
			json.NewDecoder(resp.Body).Decode(&result)
		}
		resp.Body.Close()
		if content, ok := result["response"].(string); ok {
			fmt.Println(content)
		}
		fmt.Print("> ")
	}
}
