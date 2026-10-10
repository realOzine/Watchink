//go:build ignore

// mcpsmoke drives `watchink mcp` the way an MCP client does, over real stdio
// pipes with newline-delimited JSON-RPC, and fails loudly on any deviation:
//
//	initialize → notifications/initialized → tools/list → tools/call
//	list_videos → close stdin → the server exits by itself
//
// It checks, without a real MCP client, that
// a console-subsystem executable answers on stdio pipes. The tool call makes
// the server find or start the engine, so the whole chain is exercised. It is
// not a substitute for trying a real MCP client.
//
//	go run scripts/mcpsmoke.go <path to watchink> [extra args for `mcp`]
//
// Set WATCHINK_DATA_DIR to keep the run away from real data.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type message struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/mcpsmoke.go <path to watchink> [mcp args]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mcpsmoke: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("mcpsmoke: PASS")
}

func run(exe string, extra []string) error {
	cmd := exec.Command(exe, append([]string{"mcp"}, extra...)...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	send := func(v string) error {
		_, err := io.WriteString(stdin, v+"\n")
		return err
	}
	// reply waits for the response with the given id; anything else on
	// stdout must still be JSON-RPC (a stray log line would break clients).
	reply := func(id int, within time.Duration) (json.RawMessage, error) {
		deadline := time.After(within)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return nil, fmt.Errorf("server closed stdout while waiting for response %d", id)
				}
				var m message
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					return nil, fmt.Errorf("stdout carries a non-JSON line %q: %v", line, err)
				}
				if m.ID == nil || *m.ID != id {
					continue // a notification or a server request
				}
				if len(m.Error) > 0 && string(m.Error) != "null" {
					return nil, fmt.Errorf("response %d is an error: %s", id, m.Error)
				}
				return m.Result, nil
			case <-deadline:
				return nil, fmt.Errorf("no response %d within %v", id, within)
			}
		}
	}

	// 1. initialize
	if err := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcpsmoke","version":"0"}}}`); err != nil {
		return err
	}
	res, err := reply(1, 20*time.Second)
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name, Version string
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res, &init); err != nil || init.ProtocolVersion == "" || init.ServerInfo.Name == "" {
		return fmt.Errorf("unexpected initialize result: %s", res)
	}
	fmt.Printf("initialize: %s %s, protocol %s\n", init.ServerInfo.Name, init.ServerInfo.Version, init.ProtocolVersion)
	if err := send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return err
	}

	// 2. tools/list
	if err := send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); err != nil {
		return err
	}
	if res, err = reply(2, 20*time.Second); err != nil {
		return err
	}
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("unexpected tools/list result: %s", res)
	}
	names := map[string]bool{}
	for _, t := range list.Tools {
		names[t.Name] = true
	}
	for _, want := range []string{"import_video", "get_job", "cancel_job", "retry_job", "list_videos", "get_note", "get_transcript", "export_note"} {
		if !names[want] {
			return fmt.Errorf("tools/list lacks %s: %s", want, res)
		}
	}
	fmt.Printf("tools/list: %d tools\n", len(list.Tools))

	// 3. A tool call: the server connects to the engine, starting it when
	// none is running (cold start can take a few seconds).
	if err := send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_videos","arguments":{}}}`); err != nil {
		return err
	}
	if res, err = reply(3, 60*time.Second); err != nil {
		return err
	}
	var call struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &call); err != nil || call.IsError {
		return fmt.Errorf("list_videos failed: %s", res)
	}
	if !strings.Contains(string(res), "videos") {
		return fmt.Errorf("list_videos result has no videos member: %s", res)
	}
	fmt.Println("tools/call list_videos: ok")

	// 4. A client that goes away closes stdin; the server must exit.
	stdin.Close()
	done := make(chan error, 1)
	go func() {
		for range lines {
		}
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("server exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		return fmt.Errorf("server did not exit within 15 s of stdin closing")
	}
	fmt.Println("exit after stdin closed: ok")
	return nil
}
