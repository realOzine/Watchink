// Command toolcheck is a DEV-ONLY harness that drives the real tools.Manager
// against the real manifest (S-06 / S-07 evidence). It is not part of the
// product and is the one place under internal/tools that uses os/exec: the
// engine injects a runner backed by internal/procx instead (docs/v0.md §4.3).
//
//	go run ./internal/tools/cmd/toolcheck -data .devdata -install all
//	go run ./internal/tools/cmd/toolcheck -data .devdata -status
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"watchink/internal/app"
	"watchink/internal/tools"
)

type mirrorFlag map[string]string

func (m mirrorFlag) String() string { return fmt.Sprint(map[string]string(m)) }
func (m mirrorFlag) Set(v string) error {
	host, base, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want host=baseURL")
	}
	m[host] = base
	return nil
}

func main() {
	mirrors := mirrorFlag{}
	data := flag.String("data", "", "data directory (required)")
	install := flag.String("install", "", "comma-separated manifest names, or \"all\"")
	status := flag.Bool("status", false, "print the status as JSON")
	update := flag.String("update", "", "update a tool to its latest upstream release (yt-dlp)")
	rollback := flag.String("rollback", "", "roll a tool back to its previous version")
	imp := flag.String("import", "", "name=file: import a manually downloaded file")
	proxy := flag.String("proxy", "", "proxy URL (empty: environment)")
	asr := flag.String("asr", "", "ASR model for -status (empty: platform default)")
	vad := flag.Bool("vad", true, "VAD setting for -status")
	flag.Var(mirrors, "mirror", "host=baseURL (repeatable)")
	flag.Parse()
	if *data == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	run := func(ctx context.Context, path string, args ...string) ([]byte, error) {
		start := time.Now()
		out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		fmt.Printf("  smoke: %s %s -> err=%v in %s: %s\n", path, strings.Join(args, " "), err, time.Since(start).Round(time.Millisecond), first)
		return out, err
	}
	m, err := tools.New(tools.Options{
		DataDir: *data, Exec: run, Mirrors: mirrors, Proxy: *proxy,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	check(err)

	if *imp != "" {
		name, file, _ := strings.Cut(*imp, "=")
		check(m.Import(ctx, name, file))
		fmt.Println("imported", name)
	}
	if *install != "" {
		var names []string
		if *install == "all" {
			for _, it := range m.Status("", true).Items {
				names = append(names, it.Name)
			}
		} else {
			names = strings.Split(*install, ",")
		}
		last := map[string]time.Time{}
		for _, name := range names {
			start := time.Now()
			err := m.Ensure(ctx, []string{name}, func(name string, done, total int64) {
				if time.Since(last[name]) > 2*time.Second || done == total {
					last[name] = time.Now()
					fmt.Printf("  %s: %d / %d bytes\n", name, done, total)
				}
			})
			check(err)
			fmt.Printf("ok %s (%s)\n", name, time.Since(start).Round(time.Millisecond))
		}
	}
	if *update != "" {
		v, err := m.UpdateTool(ctx, *update)
		check(err)
		fmt.Println("updated", *update, "to", v)
	}
	if *rollback != "" {
		v, err := m.Rollback(*rollback)
		check(err)
		fmt.Println("rolled back", *rollback, "to", v)
	}
	if *status {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(m.Status(*asr, *vad))
	}
}

func check(err error) {
	if err == nil {
		return
	}
	if e := app.AsError(err); e != nil {
		data, _ := json.Marshal(e)
		fmt.Fprintf(os.Stderr, "error: %s\n", data)
	}
	os.Exit(1)
}
