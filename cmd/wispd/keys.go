package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arugula-salad/wisp/internal/server"
)

// `wispd keys`: API keys, managed over the operator socket like images. The
// socket is the way in that no leaked key opens: whoever can use it can read
// the root token already.

const keysUsage = `usage: wispd keys <command> [--data <dir>]

  create <name> [--scope admin|read]   make a key and print it; it is never shown again
  list [--json]                        the keys, without their secrets
  revoke <id|name>                     delete a key; requests carrying it fail at once

admin keys can do everything the root token (<data>/token) can; read keys can
make GET requests only, with no WebSockets (so no exec, proxy or /control).
`

func runKeys(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(os.Stderr, keysUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("keys "+verb, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	scope := fs.String("scope", server.ScopeAdmin, "admin or read (create)")
	var pos []string
	rest := args[1:]
	for {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	abs, err := filepath.Abs(*data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fail := func(err error) int {
		if noDaemon(err) {
			fmt.Fprintf(os.Stderr, "no wispd is running on %s; keys %s needs one\n", abs, verb)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	client := operatorClient(abs, 10*time.Second)
	var resp *http.Response
	switch verb {
	case "list", "ls":
		resp, err = client.Get("http://wispd/keys")
	case "create":
		if len(pos) != 1 {
			fmt.Fprint(os.Stderr, keysUsage)
			return 2
		}
		body, _ := json.Marshal(map[string]string{"name": pos[0], "scope": *scope})
		resp, err = client.Post("http://wispd/keys", "application/json", bytes.NewReader(body))
	case "revoke", "rm":
		if len(pos) != 1 {
			fmt.Fprint(os.Stderr, keysUsage)
			return 2
		}
		req, _ := http.NewRequest(http.MethodDelete, "http://wispd/keys/"+url.PathEscape(pos[0]), nil)
		resp, err = client.Do(req)
	default:
		fmt.Fprint(os.Stderr, keysUsage)
		return 2
	}
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fail(apiError(resp))
	}
	if *asJSON {
		io.Copy(os.Stdout, resp.Body)
		return 0
	}
	switch verb {
	case "list", "ls":
		var keys []server.APIKey
		if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
			return fail(err)
		}
		printKeys(os.Stdout, keys)
	case "create":
		var k server.NewAPIKey
		if err := json.NewDecoder(resp.Body).Decode(&k); err != nil {
			return fail(err)
		}
		fmt.Fprintf(os.Stderr, "created %s key %q (id %s). This is the only time it is shown:\n", k.Scope, k.Name, k.ID)
		fmt.Println(k.Key)
	default:
		var k server.APIKey
		if err := json.NewDecoder(resp.Body).Decode(&k); err != nil {
			return fail(err)
		}
		fmt.Printf("revoked %q (id %s)\n", k.Name, k.ID)
	}
	return 0
}

func printKeys(w io.Writer, keys []server.APIKey) {
	if len(keys) == 0 {
		fmt.Fprintln(w, "no API keys (the root token in <data>/token always works)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tCREATED\tLAST USED")
	for _, k := range keys {
		used := "never"
		if k.LastUsedAt != nil {
			used = k.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Scope, k.CreatedAt.Local().Format("2006-01-02 15:04"), used)
	}
	tw.Flush()
}
