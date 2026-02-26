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

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/lxi1400/GoTitle"
	"gopkg.in/natefinch/lumberjack.v2"
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

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#444444")).
			Faint(true)
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

type addTorrentMsg struct {
	t *torrent.Torrent
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

	case addTorrentMsg:
		m.mu.Lock()
		m.torrents = append(m.torrents, msg.t)
		m.mu.Unlock()
		return m, nil

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
	} else if overall >= 1.0 {
		statusText = "COMPLETED"
	}

	s.WriteString(strings.Repeat("─", m.width))
	s.WriteString("\n")
	s.WriteString(fmt.Sprintf("TOTAL: %3.0f%%  D: %s/s  U: %s/s\n",
		overall*100, humanBytes(int64(totalDownRate)), humanBytes(int64(totalUpRate))))
	s.WriteString(fmt.Sprintf("Status: %s\n", statusStyle.Foreground(lipgloss.Color(statusColor(m.paused, overall >= 1.0))).Render(statusText)))
	s.WriteString(infoStyle.Render("\n[p]ause/resume  [q]uit\n"))

	footer := fmt.Sprintf("© %d Wisdom Jere Github@izzjere", now.Year())
	s.WriteString(footerStyle.Render(footer))

	return s.String()
}

func statusColor(paused bool, completed bool) string {
	if paused {
		return "#FFFF00" // Yellow
	}
	if completed {
		return "#00FFFF" // Cyan (or another color for completed)
	}
	return "#00FF00" // Green
}

// --- Torrent Logic ---

func main() {
	var (
		outDir       string
		sessionDir   string
		inputDir     string
		progressDir  string
		completedDir string
		logFile      string
		maxConns     int
		enableSeed   bool
		disableUTP   bool
		disableIPv6  bool
	)
	_, _ = title.SetTitle("utorr Downloader")

	flag.StringVar(&outDir, "o", "downloads", "Output/download directory")
	flag.StringVar(&sessionDir, "session", "session", "Directory to store session/resume data")
	flag.StringVar(&inputDir, "input", "input", "Input directory for new .torrent or .magnet files")
	flag.StringVar(&progressDir, "progress", "progress", "Directory for files in progress")
	flag.StringVar(&completedDir, "completed", "completed", "Directory for completed .torrent/.magnet files")
	flag.StringVar(&logFile, "log", "logs/utorr.log", "Path to log file")
	flag.IntVar(&maxConns, "max-conns", 80, "Max established peer connections per torrent")
	flag.BoolVar(&enableSeed, "seed", false, "Seed after download completes")
	flag.BoolVar(&disableUTP, "disable-utp", false, "Disable uTP (Micro Transport Protocol)")
	flag.BoolVar(&disableIPv6, "disable-ipv6", false, "Disable IPv6")
	flag.Usage = func() {
		name := filepath.Base(os.Args[0])
		_, _ = fmt.Fprintf(os.Stderr, "utorr - A secure, fast, multi-threaded torrent downloader with a TUI.\n\n")
		_, _ = fmt.Fprintf(os.Stderr, "Usage:\n")
		_, _ = fmt.Fprintf(os.Stderr, "  %s [options] [magnet-link|torrent-file ...]\n\n", name)
		_, _ = fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		_, _ = fmt.Fprintf(os.Stderr, "\nInteractive Commands (in TUI):\n")
		_, _ = fmt.Fprintf(os.Stderr, "  p  Toggle pause/resume for all torrents\n")
		_, _ = fmt.Fprintf(os.Stderr, "  q  Quit gracefully\n\n")
	}
	flag.Parse()
	args := flag.Args()
	if logFile == "" {
		logFile = "logs/utorr.log"
	}
	logger := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    10,   // megabytes
		MaxBackups: 7,    // keep 7 days of logs
		MaxAge:     28,   // days
		Compress:   true, // disabled by default
		LocalTime:  true,
	}
	log.SetOutput(logger)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting utorr...")

	mustMkdirAll(outDir)
	mustMkdirAll(sessionDir)
	mustMkdirAll(inputDir)
	mustMkdirAll(progressDir)
	mustMkdirAll(completedDir)

	cfg := torrent.NewDefaultClientConfig()
	cfg.NoUpload = !enableSeed
	cfg.DataDir = outDir
	cfg.DisableIPv6 = disableIPv6
	cfg.DisableUTP = disableUTP
	cfg.Seed = enableSeed
	cfg.HalfOpenConnsPerTorrent = 8
	cfg.EstablishedConnsPerTorrent = maxConns
	cfg.AcceptPeerConnections = true

	torrentLogger := alog.NewLogger()
	torrentLogger.SetHandlers(alog.StreamHandler{
		W:   logger,
		Fmt: alog.LineFormatter,
	})
	cfg.Logger = torrentLogger

	// Configure persistent storage for resume data
	pc, err := storage.NewBoltPieceCompletion(sessionDir)
	if err != nil {
		log.Printf("Error creating piece completion: %v, using default", err)
	} else {
		// NewFileOpts expects NewFileClientOpts which implements ClientImplCloser
		cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
			ClientBaseDir:   outDir,
			PieceCompletion: pc,
		})
	}

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("failed creating client: %v", err)
	}
	// Note: We'll close this client explicitly at the end of main() to ensure all data is flushed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var added []*torrent.Torrent
	for _, in := range args {
		t, e := addInput(ctx, cl, in)
		if e == nil {
			added = append(added, t)
			log.Printf("Added torrent from command line: %s", in)
		} else {
			log.Printf("Error adding %s: %v", in, e)
		}
	}

	for _, t := range added {
		go func(t *torrent.Torrent) {
			<-t.GotInfo()
			t.DownloadAll()
			t.SetMaxEstablishedConns(maxConns)
			log.Printf("Started downloading: %s", t.Name())
		}(t)
	}

	prog := progress.New(progress.WithDefaultGradient())
	model := &torrentModel{
		client:   cl,
		torrents: added,
		ratePrev: make(map[*torrent.Torrent]rateState),
		progress: prog,
	}

	p := tea.NewProgram(model, tea.WithAltScreen())

	// Start input directory goroutine
	go watchInputDirectory(ctx, cl, inputDir, progressDir, completedDir, p, maxConns)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running TUI: %v\n", err)
		os.Exit(1)
	}

	// Ensure graceful shutdown
	fmt.Println("Shutting down gracefully, flushing data...")

	// Explicitly close the client before returning to ensure all data is flushed to disk.
	cl.Close()
	fmt.Println("Done.")
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

func watchInputDirectory(ctx context.Context, cl *torrent.Client, inputDir, progressDir, completedDir string, p *tea.Program, maxConns int) {
	processed := make(map[string]bool)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// 1. Initial scan of progress directory to resume files
	progressFiles, err := os.ReadDir(progressDir)
	if err == nil {
		for _, f := range progressFiles {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(progressDir, f.Name())
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if ext == ".torrent" || ext == ".magnet" {
				t, e := addFile(cl, path, ext)
				if e == nil {
					log.Printf("Resumed torrent from progress directory: %s", path)
					startTorrent(t, maxConns, path, progressDir, completedDir, p)
				} else {
					log.Printf("Error resuming %s: %v", path, e)
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			files, err := os.ReadDir(inputDir)
			if err != nil {
				log.Printf("Error reading input directory: %v", err)
				continue
			}

			for _, f := range files {
				if f.IsDir() {
					continue
				}
				path := filepath.Join(inputDir, f.Name())
				if processed[path] {
					continue
				}

				ext := strings.ToLower(filepath.Ext(f.Name()))
				if ext == ".torrent" || ext == ".magnet" {
					log.Printf("Found new file in input directory: %s", f.Name())
					t, e := addFile(cl, path, ext)

					if e != nil {
						log.Printf("Error adding torrent from %s: %v", path, e)
					} else {
						// Move to progress folder
						newPath := filepath.Join(progressDir, f.Name())
						if err := os.Rename(path, newPath); err != nil {
							log.Printf("Error moving %s to %s: %v", path, newPath, err)
							// If move fails, we still proceed but it won't be resumable correctly if app restarts before next move
						} else {
							path = newPath
						}

						log.Printf("Added torrent from input directory: %s", path)
						startTorrent(t, maxConns, path, progressDir, completedDir, p)
					}
					processed[path] = true
				}
			}
		}
	}
}

func addFile(cl *torrent.Client, path string, ext string) (*torrent.Torrent, error) {
	if ext == ".torrent" {
		mi, err := metainfo.LoadFromFile(path)
		if err != nil {
			return nil, err
		}
		return cl.AddTorrent(mi)
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		magnet := strings.TrimSpace(string(data))
		if !isMagnet(magnet) {
			return nil, fmt.Errorf("invalid magnet link in %s", path)
		}
		return cl.AddMagnet(magnet)
	}
}

func startTorrent(t *torrent.Torrent, maxConns int, sourcePath string, progressDir string, completedDir string, p *tea.Program) {
	go func() {
		<-t.GotInfo()
		t.DownloadAll()
		t.SetMaxEstablishedConns(maxConns)
		log.Printf("Started downloading: %s", t.Name())

		// Monitor completion
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-t.Closed():
					return
				case <-ticker.C:
					if t.BytesCompleted() == t.Length() {
						log.Printf("Torrent completed: %s", t.Name())
						// Move source file to completed folder
						fileName := filepath.Base(sourcePath)
						destPath := filepath.Join(completedDir, fileName)
						if err := os.Rename(sourcePath, destPath); err != nil {
							log.Printf("Error moving completed source %s to %s: %v", sourcePath, destPath, err)
						} else {
							log.Printf("Moved completed source to: %s", destPath)
						}
						return
					}
				}
			}
		}()
	}()
	p.Send(addTorrentMsg{t: t})
}

func trimEllipsis(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
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
