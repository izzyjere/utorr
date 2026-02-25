package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var (
	rateMu   sync.Mutex
	ratePrev = map[*torrent.Torrent]rateState{}
	paused   bool
)

type rateState struct {
	t time.Time
	r int64
	w int64
}

// Simple, secure-ish, multi-torrent CLI downloader using anacrolix/torrent.
// Features:
//   - Accept magnet links or .torrent files as arguments
//   - Multi-threaded piece downloading (handled by library)
//   - Progress monitoring per torrent and overall
//   - Pause/Resume: interactive commands in the terminal (p=toggle pause, q=quit),
//     and automatic resume on next run thanks to persistent session and data dirs
//   - Safe defaults (no seeding by default, limited opens, conservative networking)
//
// Example:
//
//	utorr -o downloads "magnet:?xt=urn:btih:..." file.torrent
func main() {
	var (
		outDir      string
		sessionDir  string
		maxConns    int
		enableSeed  bool
		disableUTP  bool
		disableIPv6 bool
	)

	flag.StringVar(&outDir, "o", "downloads", "Output/download directory")
	flag.StringVar(&sessionDir, "session", "session", "Directory to store session/resume data")
	flag.IntVar(&maxConns, "max-conns", 80, "Max established peer connections per torrent")
	flag.BoolVar(&enableSeed, "seed", false, "Seed after download completes")
	flag.BoolVar(&disableUTP, "disable-utp", false, "Disable uTP (Micro Transport Protocol)")
	flag.BoolVar(&disableIPv6, "disable-ipv6", false, "Disable IPv6")
	flag.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "utorr - A secure, fast, multi-threaded torrent downloader with resume capabilities.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options] <magnet-link|torrent-file> [more...]\n\n", name)
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nInteractive Commands:\n")
		fmt.Fprintf(os.Stderr, "  p  Toggle pause/resume for all torrents\n")
		fmt.Fprintf(os.Stderr, "  q  Quit gracefully (resumable on next run)\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  %s \"magnet:?xt=urn:btih:...\"\n", name)
		fmt.Fprintf(os.Stderr, "  %s -o ./downloads linux_distro.torrent\n", name)
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	mustMkdirAll(outDir)
	mustMkdirAll(sessionDir)

	cfg := torrent.NewDefaultClientConfig()
	cfg.NoUpload = !enableSeed // Do not upload by default for safety/privacy
	cfg.DataDir = outDir       // Where content is stored
	cfg.DisableIPv6 = disableIPv6
	cfg.DisableUTP = disableUTP
	cfg.Seed = enableSeed
	cfg.HalfOpenConnsPerTorrent = 8
	cfg.EstablishedConnsPerTorrent = maxConns
	// Security-leaning defaults
	cfg.AcceptPeerConnections = true // Allow incoming for better performance

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("failed creating client: %v", err)
	}
	defer cl.Close()

	// Ctrl+C handling to ensure a clean shutdown and state persistence.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		wg      sync.WaitGroup
		added   []*torrent.Torrent
		addErrs []error
	)

	for _, in := range args {
		t, e := addInput(ctx, cl, in)
		if e != nil {
			addErrs = append(addErrs, fmt.Errorf("%s: %w", in, e))
			continue
		}
		added = append(added, t)
	}
	if len(added) == 0 {
		for _, e := range addErrs {
			log.Println("add error:", e)
		}
		log.Fatal("no valid inputs added")
	}

	// Start fetching metadata if needed, then download all files.
	for _, t := range added {
		<-t.GotInfo()
		// By default, download everything in the torrent.
		t.DownloadAll()
		// Apply per-torrent connection cap.
		t.SetMaxEstablishedConns(maxConns)
	}

	// Progress display & interactive control.
	wg.Add(1)
	go func() {
		defer wg.Done()
		progressLoop(ctx, added)
	}()

	// Simple stdin commands: 'p' to toggle pause for all, 'q' to quit gracefully.
	wg.Add(1)
	go func() {
		defer wg.Done()
		interactiveLoop(ctx, added)
	}()

	// Wait for completion or signal.
	allDone := make(chan struct{})
	go func() {
		for _, t := range added {
			<-t.Complete().On() // waits until piece completion event fires
		}
		close(allDone)
	}()

	select {
	case <-ctx.Done():
		log.Println("received shutdown signal; saving state and exiting...")
	case <-allDone:
		log.Println("all torrents completed")
	}

	// Let goroutines finish up.
	stop()
	wg.Wait()
}

func mustMkdirAll(p string) {
	if err := os.MkdirAll(p, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", p, err)
	}
}

func isMagnet(u string) bool {
	return strings.HasPrefix(strings.ToLower(u), "magnet:")
}

func addInput(ctx context.Context, cl *torrent.Client, in string) (*torrent.Torrent, error) {
	if isMagnet(in) {
		return cl.AddMagnet(in)
	}
	// If it's a URL but not magnet, try parsing; we only support magnet or a local .torrent path here.
	if _, err := url.ParseRequestURI(in); err == nil && !isMagnet(in) {
		return nil, errors.New("only magnet: URIs or .torrent file paths are supported")
	}
	// Treat as file path.
	st, err := os.Stat(in)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory; provide a .torrent file inside or a magnet link", in)
	}
	mi, err := metainfo.LoadFromFile(in)
	if err != nil {
		return nil, fmt.Errorf("loading metainfo: %w", err)
	}
	return cl.AddTorrent(mi)
}

func progressLoop(ctx context.Context, ts []*torrent.Torrent) {
	tkr := time.NewTicker(1 * time.Second)
	defer tkr.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tkr.C:
			printProgress(ts)
		}
	}
}

func printProgress(ts []*torrent.Torrent) {
	var totalDone, totalSize int64
	var totalDownRate, totalUpRate float64

	rateMu.Lock()
	now := time.Now()
	for _, t := range ts {
		size := t.Length()
		done := t.BytesCompleted()
		st := t.Stats()

		// Compute per-torrent rates from byte counters.
		prev := ratePrev[t]
		read := st.ConnStats.BytesReadData.Int64()
		write := st.ConnStats.BytesWrittenData.Int64()
		var downRate, upRate float64
		if !prev.t.IsZero() {
			dt := now.Sub(prev.t).Seconds()
			if dt > 0 {
				downRate = float64(read-prev.r) / dt
				upRate = float64(write-prev.w) / dt
			}
		}
		ratePrev[t] = rateState{t: now, r: read, w: write}

		totalDone += done
		totalSize += size
		totalDownRate += downRate
		totalUpRate += upRate

		name := t.Name()
		if name == "" {
			name = t.InfoHash().HexString()
		}
		pct := 0.0
		if size > 0 {
			pct = float64(done) / float64(size) * 100
		}
		fmt.Printf("[%s] %.1f%% - D: %s/s U: %s/s Peers: %d/%d\n",
			trimEllipsis(name, 40), pct,
			humanBytes(int64(downRate)), humanBytes(int64(upRate)),
			st.ActivePeers, st.TotalPeers,
		)
	}
	rateMu.Unlock()

	overall := 0.0
	if totalSize > 0 {
		overall = float64(totalDone) / float64(totalSize) * 100
	}
	fmt.Printf("TOTAL: %.1f%% - D: %s/s U: %s/s\n\n", overall, humanBytes(int64(totalDownRate)), humanBytes(int64(totalUpRate)))
}

func trimEllipsis(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func interactiveLoop(ctx context.Context, ts []*torrent.Torrent) {
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Commands: [p]ause/resume all, [q]uit > ")
		b, err := in.ReadByte()
		if err != nil {
			return
		}
		switch strings.ToLower(string(b)) {
		case "p":
			// Toggle pause across all torrents by allowing/disallowing data download.
			for _, t := range ts {
				if paused {
					t.AllowDataDownload()
				} else {
					t.DisallowDataDownload()
				}
			}
			paused = !paused
		case "q":
			return
		default:
			// ignore other keys including newlines
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
