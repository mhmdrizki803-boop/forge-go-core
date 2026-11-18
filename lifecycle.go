package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// newListener binds a TCP listener on addr (":0" picks a free port).
func newListener(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

type netTCPAddr = net.TCPAddr

// register POSTs {"port":N} to the env until it accepts it.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// waitForDone polls GET /done until it answers {"done":true}.
func waitForDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 250 * time.Millisecond
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var out struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(raw, &out) == nil && out.Done {
					return
				}
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}
