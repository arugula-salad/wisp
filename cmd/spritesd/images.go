package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/server"
)

// `spritesd images`: the cache of sprite disks built from container images.
// It talks to the running daemon over the operator socket, like status, since
// the daemon owns the cache; list alone also works with no daemon.

const imagesUsage = `usage: spritesd images <command> [--data <dir>]

  pull <ref>        pull the image (again) and build its sprite disk; a moved tag replaces the old disk
  list [--json]     the cached images
  rm <ref|id>       delete a cached disk; sprites made from it are unaffected

Sprites are created from an image with "from": {"image": "<ref>"} on POST /v1/sprites.
From inside a sprite (sprite-env sprites create --image) only cached images can be used.
`

func operatorClient(dataDir string, timeout time.Duration) *http.Client {
	path := filepath.Join(dataDir, server.StatusSocket)
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}}
}

func noDaemon(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func runImages(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(os.Stderr, imagesUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("images "+verb, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON (list)")
	// Flags may come before or after the positional argument.
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
			fmt.Fprintf(os.Stderr, "no spritesd is running on %s; images %s needs one\n", abs, verb)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	switch verb {
	case "list", "ls":
		var imgs []server.CachedImage
		resp, err := operatorClient(abs, 30*time.Second).Get("http://spritesd/images")
		switch {
		case err == nil:
			defer resp.Body.Close()
			if err := json.NewDecoder(resp.Body).Decode(&imgs); err != nil {
				return fail(err)
			}
		case noDaemon(err):
			imgs = server.OfflineImages(abs)
		default:
			return fail(err)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(imgs)
			return 0
		}
		printImages(os.Stdout, imgs)
		return 0

	case "pull":
		if len(pos) != 1 {
			fmt.Fprint(os.Stderr, imagesUsage)
			return 2
		}
		// No client timeout: a large image takes minutes, and the daemon streams progress.
		resp, err := operatorClient(abs, 0).Post("http://spritesd/images/pull?ref="+url.QueryEscape(pos[0]), "", nil)
		if err != nil {
			return fail(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fail(apiError(resp))
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if b, ok := strings.CutPrefix(line, "ok "); ok {
				var img server.CachedImage
				if json.Unmarshal([]byte(b), &img) == nil {
					printImages(os.Stdout, []server.CachedImage{img})
				}
				return 0
			}
			if msg, ok := strings.CutPrefix(line, "error "); ok {
				fmt.Fprintln(os.Stderr, "pull failed:", msg)
				return 1
			}
			fmt.Fprintln(os.Stderr, line)
		}
		fmt.Fprintln(os.Stderr, "the daemon hung up before the pull finished; it continues in the background (see spritesd images list)")
		return 1

	case "rm", "remove", "delete":
		if len(pos) != 1 {
			fmt.Fprint(os.Stderr, imagesUsage)
			return 2
		}
		req, _ := http.NewRequest(http.MethodDelete, "http://spritesd/images?key="+url.QueryEscape(pos[0]), nil)
		resp, err := operatorClient(abs, 30*time.Second).Do(req)
		if err != nil {
			return fail(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fail(apiError(resp))
		}
		var img server.CachedImage
		json.NewDecoder(resp.Body).Decode(&img)
		fmt.Printf("removed %.12s (%s)\n", img.ID, strings.Join(img.Refs, ", "))
		return 0
	}
	fmt.Fprint(os.Stderr, imagesUsage)
	return 2
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct{ Message string }
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return fmt.Errorf("daemon answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func printImages(w io.Writer, imgs []server.CachedImage) {
	if len(imgs) == 0 {
		fmt.Fprintln(w, "no cached images")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREFS\tSHELL\tSUDO\tUID\tIMAGE\tDISK\tBUILT\tLAST USED")
	for _, img := range imgs {
		shell := img.Account.Shell
		if shell == "" {
			shell = "none"
		}
		sudo := "stand-in"
		if img.Account.Sudo {
			sudo = "image's"
		}
		used := "never"
		if img.LastUsedAt != nil {
			used = ago(*img.LastUsedAt)
		}
		fmt.Fprintf(tw, "%.12s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", img.ID, strings.Join(img.Refs, ","), shell, sudo,
			img.Account.UID, size(img.ImageBytes), size(img.DiskBytes), ago(img.BuiltAt), used)
	}
	tw.Flush()
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
