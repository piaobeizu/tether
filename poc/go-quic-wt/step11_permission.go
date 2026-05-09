//go:build step11

// Step 11: PoC-PERM — verify cc stream-json permission flow.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type runResult struct {
	mode       string
	events     []map[string]interface{}
	eventTypes map[string]int
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func runProbe(ctx context.Context, permMode, prompt string, timeout time.Duration) *runResult {
	args := []string{"--print", "--verbose",
		"--output-format", "stream-json",
		"--input-format", "stream-json"}
	if permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(subCtx, "claude", args...)
	if os.Geteuid() == 0 {
		cmd.Env = append(os.Environ(), "IS_SANDBOX=1")
	}

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("  start error: %v", err)
		return &runResult{mode: permMode, eventTypes: map[string]int{}}
	}

	res := &runResult{mode: permMode, eventTypes: map[string]int{}}
	var mu sync.Mutex

	go func() {
		msg := map[string]interface{}{
			"type": "user",
			"message": map[string]interface{}{
				"role":    "user",
				"content": prompt,
			},
		}
		_ = json.NewEncoder(stdin).Encode(msg)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 100*1024*1024)
	for scanner.Scan() {
		var ev map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		mu.Lock()
		res.events = append(res.events, ev)
		key := getStr(ev, "type")
		if sub := getStr(ev, "subtype"); sub != "" {
			key += "/" + sub
		}
		res.eventTypes[key]++
		mu.Unlock()
	}
	stdin.Close()
	_ = cmd.Wait()
	return res
}

func main() {
	log.SetFlags(0)
	log.Println("==Step 11: PoC-PERM==")
	log.Println()

	rootCtx := context.Background()
	prompt := "Run the bash command `echo hello-from-permcheck` to print a string. Use the Bash tool. Do not skip."

	log.Println("[run 1] permission_mode=default")
	r1 := runProbe(rootCtx, "default", prompt, 30*time.Second)
	log.Printf("  events: %d", len(r1.events))
	for k, v := range r1.eventTypes {
		log.Printf("    %-40s %d", k, v)
	}

	cands := []map[string]interface{}{}
	for _, ev := range r1.events {
		t := strings.ToLower(getStr(ev, "type") + " " + getStr(ev, "subtype"))
		if strings.Contains(t, "perm") || strings.Contains(t, "approval") || strings.Contains(t, "request") {
			cands = append(cands, ev)
		}
	}
	log.Printf("  permission-shaped candidates: %d", len(cands))
	for i, ev := range cands {
		if i >= 5 {
			log.Printf("    ... (%d more)", len(cands)-5)
			break
		}
		b, _ := json.Marshal(ev)
		s := string(b)
		if len(s) > 400 {
			s = s[:400] + "..."
		}
		log.Printf("    [%d] %s", i+1, s)
	}

	log.Println()
	log.Println("[run 2] permission_mode=bypassPermissions")
	r2 := runProbe(rootCtx, "bypassPermissions", prompt, 30*time.Second)
	log.Printf("  events: %d", len(r2.events))
	for k, v := range r2.eventTypes {
		log.Printf("    %-40s %d", k, v)
	}

	log.Println()
	log.Println("==Event diff (decreased in bypass mode)==")
	for k, v := range r1.eventTypes {
		if r2.eventTypes[k] < v {
			log.Printf("  %-40s -%d (was %d, now %d)", k, v-r2.eventTypes[k], v, r2.eventTypes[k])
		}
	}

	log.Println()
	log.Println("==FINDINGS==")
	hasPerm := false
	for k := range r1.eventTypes {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "perm") || strings.Contains(lk, "approval") || strings.Contains(lk, "request") {
			log.Printf("  + explicit permission event type observed: %q (count=%d)", k, r1.eventTypes[k])
			hasPerm = true
		}
	}
	if !hasPerm {
		log.Println("  ! NO event type with 'perm'/'approval'/'request' substring observed")
	}
	fmt.Println()
}
