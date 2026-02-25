package main

import (
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
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- Bubble Tea TUI ---

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1).
			MarginBottom(1)

	infoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#626262"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF0000"))

	statusStyle = lipgloss.NewStyle().
			Bold(true)
)

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Every(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

type torrentModel struct {
	client   *torrent.Client
	torrents []*torrent.Torrent
	paused   bool
	err      error
	quitting bool
	width    int

	ratePrev map[*torrent.Torrent]rateState
	progress progress.Model
	mu       sync.Mutex
}

type rateState struct {
	t time.Time
	r int64
	w int64
}

func (m *torrentModel) Init() tea.Cmd {
	return tick()
}

func (m *torrentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "p":
			m.mu.Lock()
			m.paused = !m.paused
			for _, t := range m.torrents {
				if m.paused {
					t.DisallowDataDownload()
				} else {
					t.AllowDataDownload()
				}
			}
			m.mu.Unlock()
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.progress.Width = msg.Width - 10
		if m.progress.Width > 80 {
			m.progress.Width = 80
		}

	case tickMsg:
		if m.quitting {
			return m, nil
		}
		return m, tick()

	case error:
		m.err = msg
		return m, nil
	}

	return m, nil
}

func (m *torrentModel) View() string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error: %v", m.err))
	}
	if m.quitting {
		return "Shutting down gracefully..."
	}

	var s strings.Builder

	s.WriteString(titleStyle.Render("utorr Downloader"))
	s.WriteString("\n")

	var totalDone, totalSize int64
	var totalDownRate, totalUpRate float64
	now := time.Now()

	for _, t := range m.torrents {
		size := t.Length()
		done := t.BytesCompleted()
		st := t.Stats()

		m.mu.Lock()
		prev := m.ratePrev[t]
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
		m.ratePrev[t] = rateState{t: now, r: read, w: write}
		m.mu.Unlock()

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
			pct = float64(done) / float64(size)
		}

		s.WriteString(fmt.Sprintf("%s\n", trimEllipsis(name, m.width-5)))
		s.WriteString(fmt.Sprintf("%s %3.0f%%\n", m.progress.ViewAs(pct), pct*100))
		s.WriteString(infoStyle.Render(fmt.Sprintf("D: %s/s  U: %s/s  Peers: %d/%d",
			humanBytes(int64(downRate)), humanBytes(int64(upRate)),
			st.ActivePeers, st.TotalPeers)))
		s.WriteString("\n\n")
	}

	overall := 0.0
	if totalSize > 0 {
		overall = float64(totalDone) / float64(totalSize)
	}

	statusText := "RUNNING"
	if m.paused {
		statusText = "PAUSED"
	}

	s.WriteString(strings.Repeat("─", m.width))
	s.WriteString("\n")
	s.WriteString(fmt.Sprintf("TOTAL: %3.0f%%  D: %s/s  U: %s/s\n",
		overall*100, humanBytes(int64(totalDownRate)), humanBytes(int64(totalUpRate))))
	s.WriteString(fmt.Sprintf("Status: %s\n", statusStyle.Foreground(lipgloss.Color(statusColor(m.paused))).Render(statusText)))
	s.WriteString(infoStyle.Render("\n[p]ause/resume  [q]uit"))

	return s.String()
}

func statusColor(paused bool) string {
	if paused {
		return "#FFFF00" // Yellow
	}
	return "#00FF00" // Green
}

// --- Torrent Logic ---

func main() {
	var (
		outDir      string
		sessionDir  string
		maxConns    int
		enableSeed  bool
		disableUTP  bool
		disableIPv6 bool
	)

	flag.StringVar(&outDir, "o", "/utorr/downloads", "Output/download directory")
	flag.StringVar(&sessionDir, "session", "/utorr/session", "Directory to store session/resume data")
	flag.IntVar(&maxConns, "max-conns", 80, "Max established peer connections per torrent")
	flag.BoolVar(&enableSeed, "seed", false, "Seed after download completes")
	flag.BoolVar(&disableUTP, "disable-utp", false, "Disable uTP (Micro Transport Protocol)")
	flag.BoolVar(&disableIPv6, "disable-ipv6", false, "Disable IPv6")
	flag.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "utorr - A secure, fast, multi-threaded torrent downloader with a TUI.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options] <magnet-link|torrent-file> [more...]\n\n", name)
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nInteractive Commands (in TUI):\n")
		fmt.Fprintf(os.Stderr, "  p  Toggle pause/resume for all torrents\n")
		fmt.Fprintf(os.Stderr, "  q  Quit gracefully\n\n")
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
	cfg.NoUpload = !enableSeed
	cfg.DataDir = outDir
	cfg.DisableIPv6 = disableIPv6
	cfg.DisableUTP = disableUTP
	cfg.Seed = enableSeed
	cfg.HalfOpenConnsPerTorrent = 8
	cfg.EstablishedConnsPerTorrent = maxConns
	cfg.AcceptPeerConnections = true

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("failed creating client: %v", err)
	}
	defer cl.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var added []*torrent.Torrent
	for _, in := range args {
		t, e := addInput(ctx, cl, in)
		if e == nil {
			added = append(added, t)
		} else {
			fmt.Printf("Error adding %s: %v\n", in, e)
		}
	}

	if len(added) == 0 {
		log.Fatal("no valid inputs added")
	}

	for _, t := range added {
		<-t.GotInfo()
		t.DownloadAll()
		t.SetMaxEstablishedConns(maxConns)
	}

	prog := progress.New(progress.WithDefaultGradient())
	model := &torrentModel{
		client:   cl,
		torrents: added,
		ratePrev: make(map[*torrent.Torrent]rateState),
		progress: prog,
	}

	p := tea.NewProgram(model, tea.WithAltScreen())

	// Run TUI in a separate goroutine if we want to handle signals here too,
	// but Bubble Tea handles signals by default.
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running TUI: %v\n", err)
		os.Exit(1)
	}

	// Ensure graceful shutdown
	fmt.Println("Shutting down gracefully, flushing data...")
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
	if _, err := url.ParseRequestURI(in); err == nil && !isMagnet(in) {
		return nil, errors.New("only magnet: URIs or .torrent file paths are supported")
	}
	st, err := os.Stat(in)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", in)
	}
	mi, err := metainfo.LoadFromFile(in)
	if err != nil {
		return nil, fmt.Errorf("loading metainfo: %w", err)
	}
	return cl.AddTorrent(mi)
}

func trimEllipsis(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
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
