package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sdr-dialer/config"
	"sdr-dialer/dialer"
	"sdr-dialer/hubspot"
	"sdr-dialer/leads"
	"sdr-dialer/logger"
	"sdr-dialer/research"
	"sdr-dialer/session"
)

// postCallFn is called after each logged call to write back to an external system.
type postCallFn func(eng hubspot.Engagement) error

func main() {
	fileFlag := flag.String("file", "", "Path to specific CSV (skips selection menu)")
	dryRun := flag.Bool("dry-run", false, "Load and display companies, no calls, no API requests")
	noResearch := flag.Bool("no-research", false, "Skip research pipeline")
	noAircall := flag.Bool("no-aircall", false, "Skip Aircall dialing (simulate calls instead)")
	researchOnly := flag.Bool("research-only", false, "Run research on first company and print, then exit")
	fromFlag := flag.Int("from", 0, "Start from company number N (1-indexed)")
	freshFlag := flag.Bool("fresh", false, "Ignore saved session and start from beginning")
	flag.Parse()

	var cfg *config.Config
	if *dryRun || *noResearch {
		// Load all optional keys (HubSpot, Aircall) without requiring Anthropic/Browserless.
		cfg = config.LoadPartial()
	} else {
		var err error
		cfg, err = config.Load()
		if err != nil {
			os.Exit(1)
		}
	}

	// Determine source and load companies.
	var (
		companies      []*leads.Company
		csvPath        string
		csvDisplayName string
		postCall       postCallFn
	)

	if *fileFlag != "" {
		// Explicit CSV path bypasses source selection.
		csvPath = *fileFlag
		var err error
		companies, err = leads.Load(csvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ Error loading CSV: %v\n", err)
			os.Exit(1)
		}
		csvDisplayName = displayName(csvPath)
	} else {
		source := selectSource()
		switch source {
		case "quit":
			return

		case "hubspot":
			if cfg.HubSpotAccessToken == "" {
				fmt.Fprintln(os.Stderr, "✗ HUBSPOT_ACCESS_TOKEN not set. Add it to your .env file.")
				fmt.Fprintln(os.Stderr, "  See .env.example for the template.")
				os.Exit(1)
			}
			listID, listName, selectErr := selectHubSpotList(cfg.HubSpotAccessToken)
			if selectErr != nil {
				fmt.Fprintln(os.Stderr, "✗", selectErr)
				os.Exit(1)
			}
			if listID == 0 {
				return // user quit
			}
			fmt.Print("\n  Loading contacts from HubSpot...")
			var loadErr error
			companies, loadErr = hubspot.LoadContacts(cfg.HubSpotAccessToken, listID)
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "\n✗ Could not load HubSpot contacts: %v\n", loadErr)
				os.Exit(1)
			}
			fmt.Printf(" done (%d companies)\n", len(companies))
			csvPath = fmt.Sprintf("hubspot_list_%d", listID)
			csvDisplayName = listName
			token := cfg.HubSpotAccessToken
			postCall = func(eng hubspot.Engagement) error {
				return hubspot.WriteCallEngagement(token, eng)
			}

		case "csv":
			selPath, selErr := selectCSV()
			if selErr != nil {
				fmt.Fprintln(os.Stderr, selErr)
				os.Exit(1)
			}
			if selPath == "" {
				return
			}
			csvPath = selPath
			var loadErr error
			companies, loadErr = leads.Load(csvPath)
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "✗ Error loading CSV: %v\n", loadErr)
				os.Exit(1)
			}
			csvDisplayName = displayName(csvPath)
		}
	}

	total := len(companies)
	if total == 0 {
		fmt.Println("No companies found.")
		return
	}

	if *dryRun {
		fmt.Printf("SDR Dialer — Dry Run\n%s\n%d companies found:\n\n", strings.Repeat("─", 50), total)
		for i, co := range companies {
			p := co.Primary
			fmt.Printf("  %3d. %-35s %s %s (%s) — %s, %s\n",
				i+1, co.Name, p.FirstName, p.LastName, p.Title, co.City, co.Province)
		}
		return
	}

	if *researchOnly {
		co := companies[0]
		fmt.Printf("Running research for: %s\n\n", co.Name)
		ch := make(chan research.Brief, 1)
		go research.Run(co, cfg, ch)
		brief := <-ch
		printResearchBrief(brief)
		return
	}

	// Session management
	startPos := 0
	var sess *session.SessionState
	var log *logger.Logger
	var err error

	if *fromFlag > 0 {
		startPos = *fromFlag - 1
	} else if !*freshFlag {
		existing, isActive, loadErr := session.Load(csvPath)
		if loadErr == nil && isActive && existing != nil {
			action := promptResume(existing)
			switch action {
			case "r":
				startPos = existing.CurrentPosition
				log, err = logger.New(existing.ActiveLogFile)
				if err != nil {
					log, _ = logger.New(logger.LogFileName(csvPath))
				}
				_ = existing.Resume(log.Path())
				sess = existing
				sess.CSVPath = csvPath
				runLoop(companies, startPos, total, csvPath, csvDisplayName, cfg, *noResearch, *noAircall, log, sess, postCall)
				return
			case "s":
				_ = existing.Delete()
				if !isHubSpotSource(csvPath) {
					if stripped, err2 := session.RenameCSV(csvPath, ""); err2 == nil {
						csvPath = stripped
						csvDisplayName = displayName(csvPath)
					}
				}
			case "q":
				return
			}
		}
	}

	logFile := logger.LogFileName(csvPath)
	log, err = logger.New(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Could not create log file: %v\n", err)
		os.Exit(1)
	}

	sess, err = session.New(csvPath, total, logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create session: %v\n", err)
		sess = &session.SessionState{
			CSVPath:        csvPath,
			TotalCompanies: total,
			ActiveLogFile:  logFile,
			OutcomeCounts:  make(map[string]int),
		}
	}

	runLoop(companies, startPos, total, csvPath, csvDisplayName, cfg, *noResearch, *noAircall, log, sess, postCall)
}

func runLoop(
	companies []*leads.Company,
	startPos, total int,
	csvPath, csvDisplayName string,
	cfg *config.Config,
	noResearch bool,
	noAircall bool,
	log *logger.Logger,
	sess *session.SessionState,
	postCall postCallFn,
) {
	prefixApplied := isHubSpotSource(csvPath) || strings.HasPrefix(filepath.Base(csvPath), "[in progress]")
	sessionCalls := 0
	sessionStart := startPos + 1
	sessionOutcomes := make(map[string]int)

	prefetcher := research.NewPrefetcher(companies, cfg, 3, noResearch)
	if !noResearch {
		showResearchLoadingScreen(prefetcher, startPos, companies)
	}

	// briefCache lets us re-show already-fetched research when going back.
	briefCache := make(map[int]research.Brief)
	// calledAt tracks which positions had a call logged (and with what disposition)
	// so we can undo the log entry when the user goes back.
	calledAt := make(map[int]string)

	for i := startPos; i < len(companies); {
		co := companies[i]
		position := i + 1

		var ch chan research.Brief
		if cached, ok := briefCache[i]; ok {
			ch = make(chan research.Brief, 1)
			ch <- cached
		} else {
			ch = prefetcher.Chan(i)
		}

		result, contactIdx := dialer.RunCard(co, 0, position, total, csvDisplayName, ch, i > startPos, cfg, noAircall)

		// Cache the brief so going back can re-display it instantly.
		if result.Brief.WhatTheyDo != "" || result.Brief.Raw != "" {
			briefCache[i] = result.Brief
		}

		if result.Quit {
			printQuitSummary(csvDisplayName, sess, sessionCalls, sessionStart, i+1, sessionOutcomes, log.Path())
			return
		}

		if result.Back {
			// Going back: remove the previous company's log entry if it was a real call.
			prev := i - 1
			if disposition, called := calledAt[prev]; called {
				if err := log.RemoveLast(); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not remove log entry: %v\n", err)
				}
				_ = sess.Retreat(disposition)
				sessionCalls--
				sessionOutcomes[disposition]--
				delete(calledAt, prev)
			} else {
				// Previous company was skipped — still need to retreat the position counter.
				_ = sess.Retreat("")
			}
			i--
			continue
		}

		disposition := ""
		if !result.Skipped {
			disposition = result.Disposition
		}

		_ = sess.Advance(co.Name, disposition)

		// Apply [in progress] prefix on first advance (CSV mode only).
		if !prefixApplied {
			if newPath, err2 := session.RenameCSV(csvPath, "[in progress] "); err2 == nil {
				csvPath = newPath
				sess.CSVPath = csvPath
			}
			prefixApplied = true
		}

		if result.Skipped {
			i++
			continue
		}

		// Build and write local log entry.
		dialedContact := co.Contacts[contactIdx]
		entry := logger.BuildEntry(
			co,
			dialedContact,
			leads.BestPhone(dialedContact),
			result.Disposition,
			result.Sentiment,
			result.FreeText,
			result.Brief,
			result.Start,
			result.End,
		)
		if err := log.Append(entry); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not write log: %v\n", err)
		}

		// Write back to HubSpot if in HubSpot mode.
		if postCall != nil && dialedContact.HubSpotID != "" {
			eng := hubspot.Engagement{
				ContactID:      dialedContact.HubSpotID,
				Title:          entry.HsCallTitle,
				Disposition:    logger.DispositionGUID(result.Disposition),
				DispositionKey: result.Disposition,
				Duration:       entry.HsCallDuration,
				Timestamp:      entry.HsTimestamp,
				Body:           entry.HsCallBody,
			}
			if err := postCall(eng); err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠  HubSpot write-back failed: %v\n", err)
			}
		}

		calledAt[i] = result.Disposition
		sessionCalls++
		sessionOutcomes[result.Disposition]++
		i++
	}

	// Completed
	_ = sess.Complete()
	if !isHubSpotSource(csvPath) {
		if _, err2 := session.RenameCSV(csvPath, "[completed] "); err2 != nil {
			stripped, _ := session.RenameCSV(csvPath, "")
			session.RenameCSV(stripped, "[completed] ")
		}
	}
	printCompleteSummary(csvDisplayName, sess, sessionCalls, sessionStart, len(companies), sessionOutcomes, log.Path())
}

// isHubSpotSource returns true for virtual HubSpot session paths.
func isHubSpotSource(path string) bool {
	return strings.HasPrefix(path, "hubspot_list_")
}

// showResearchLoadingScreen warms the prefetch window and blocks until the
// first company's research is ready, showing a live progress spinner.
func showResearchLoadingScreen(pf *research.Prefetcher, startPos int, companies []*leads.Company) {
	ch := pf.Chan(startPos)
	warmCount := pf.WarmCount(startPos)
	name := companies[startPos].Name

	fmt.Printf("\n  Prefetching research for next %d companies...\n\n", warmCount)

	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	frame := 0
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case brief := <-ch:
			fmt.Printf("\r  ✓  Research ready — starting dial session%s\n\n",
				strings.Repeat(" ", 20))
			refilled := make(chan research.Brief, 1)
			refilled <- brief
			pf.Set(startPos, refilled)
			return
		case <-ticker.C:
			done := pf.DoneCount()
			fmt.Printf("\r  %s  %-32s [%d/%d ready]",
				spinner[frame%len(spinner)], name, done, warmCount)
			frame++
		}
	}
}

func displayName(csvPath string) string {
	base := strings.TrimSuffix(filepath.Base(csvPath), ".csv")
	for _, p := range []string{"[in progress] ", "[completed] "} {
		base = strings.TrimPrefix(base, p)
	}
	return base
}

// selectSource asks the user whether to load from HubSpot or a local CSV.
func selectSource() string {
	fmt.Print("\nSDR Dialer — Select source:\n\n")
	fmt.Println("  1  HubSpot contact list")
	fmt.Println("  2  Local CSV file")
	fmt.Println("\n  q  Quit")
	fmt.Println("\n  Press a key:")
	fmt.Print("> ")

	for {
		key, err := dialer.ReadKey()
		if err != nil {
			return "quit"
		}
		switch key {
		case '1':
			fmt.Println()
			return "hubspot"
		case '2':
			fmt.Println()
			return "csv"
		case 'q', 'Q', 3:
			fmt.Println()
			return "quit"
		}
	}
}

// selectHubSpotList fetches available lists and lets the user pick one.
// Shows 9 per page with n/p navigation. Returns (0, "", nil) if the user quits.
func selectHubSpotList(token string) (int, string, error) {
	fmt.Println("\n  Fetching HubSpot contact lists...")
	lists, err := hubspot.FetchLists(token)
	if err != nil {
		return 0, "", fmt.Errorf("could not fetch HubSpot lists: %w", err)
	}
	if len(lists) == 0 {
		return 0, "", fmt.Errorf("no contact lists found in HubSpot")
	}

	const pageSize = 9
	page := 0
	totalPages := (len(lists) + pageSize - 1) / pageSize

	printPage := func() {
		start := page * pageSize
		end := min(start+pageSize, len(lists))
		slice := lists[start:end]

		fmt.Printf("\n  Select a HubSpot list:  (page %d of %d)\n\n", page+1, totalPages)
		for i, l := range slice {
			fmt.Printf("  %d. %-45s (%d contacts)\n", i+1, l.Name, l.Size)
		}
		fmt.Println()
		if totalPages > 1 {
			if page < totalPages-1 {
				fmt.Println("  n  Next page")
			}
			if page > 0 {
				fmt.Println("  p  Previous page")
			}
		}
		fmt.Println("  q  Back")
		fmt.Printf("\n  Press a number key (1–%d):\n> ", len(slice))
	}

	printPage()

	for {
		key, err := dialer.ReadKey()
		if err != nil {
			return 0, "", nil
		}
		switch {
		case key == 'q' || key == 'Q' || key == 3:
			fmt.Println()
			return 0, "", nil
		case (key == 'n' || key == 'N') && page < totalPages-1:
			page++
			fmt.Println()
			printPage()
		case (key == 'p' || key == 'P') && page > 0:
			page--
			fmt.Println()
			printPage()
		default:
			start := page * pageSize
			end := min(start+pageSize, len(lists))
			idx := int(key-'1') + start
			if idx >= start && idx < end {
				fmt.Println()
				l := lists[idx]
				return l.ListID, l.Name, nil
			}
		}
	}
}

func selectCSV() (string, error) {
	cwd, _ := os.Getwd()
	showCompleted := false

	for {
		files, err := leads.ScanCSVFiles(cwd)
		if err != nil {
			return "", fmt.Errorf("could not scan directory: %w", err)
		}

		var fresh, inProgress, completed []leads.CSVFile
		for _, f := range files {
			switch f.Status {
			case "fresh":
				fresh = append(fresh, f)
			case "in_progress":
				inProgress = append(inProgress, f)
			case "completed":
				completed = append(completed, f)
			}
		}

		fmt.Print("\n  Select a lead list:\n\n")
		num := 1
		var displayFiles []leads.CSVFile

		for _, f := range fresh {
			coCount := countCompanies(f.Path)
			fmt.Printf("  %d. %-48s (%d companies)\n", num, f.Display, coCount)
			displayFiles = append(displayFiles, f)
			num++
		}
		for _, f := range inProgress {
			s, _, _ := session.Load(f.Path)
			if s != nil {
				fmt.Printf("  %d. [in progress] %-40s (%d companies — paused at %d/%d)\n",
					num, f.Display, s.TotalCompanies, s.CurrentPosition, s.TotalCompanies)
			} else {
				fmt.Printf("  %d. [in progress] %s\n", num, f.Display)
			}
			displayFiles = append(displayFiles, f)
			num++
		}
		if showCompleted {
			for _, f := range completed {
				fmt.Printf("  %d. [completed] %s\n", num, f.Display)
				displayFiles = append(displayFiles, f)
				num++
			}
		}

		if len(displayFiles) == 0 {
			if len(completed) > 0 && !showCompleted {
				fmt.Println("  (all lists are completed)")
			} else {
				fmt.Println("✗ No CSV files found in current directory.")
				fmt.Println("  Place your lead list CSV here and run again.")
				return "", nil
			}
		}

		fmt.Println()
		if !showCompleted && len(completed) > 0 {
			fmt.Println("  c  Show completed lists")
		}
		fmt.Println("  q  Back")
		fmt.Println("\n  Press a number key to select a list:")
		fmt.Print("> ")

		key, err := dialer.ReadKey()
		if err != nil {
			return "", nil
		}

		switch {
		case key == 'q' || key == 'Q' || key == 3:
			return "", nil
		case key == 'c' || key == 'C':
			showCompleted = true
		default:
			idx := int(key - '1')
			if idx >= 0 && idx < len(displayFiles) {
				fmt.Println()
				return displayFiles[idx].Path, nil
			}
		}
	}
}

func countCompanies(path string) int {
	companies, err := leads.Load(path)
	if err != nil {
		return 0
	}
	return len(companies)
}

func promptResume(sess *session.SessionState) string {
	name := displayName(sess.CSVPath)
	if isHubSpotSource(sess.CSVPath) {
		name = sess.CSVPath // use the raw virtual path as a fallback label
	}
	lastActive, _ := time.Parse(time.RFC3339, sess.LastActiveAt)
	dateStr := lastActive.Format("2006-01-02")

	fmt.Printf("\n  ↩  %s has a saved session.\n", name)
	fmt.Printf("     Last position: company %d of %d", sess.CurrentPosition, sess.TotalCompanies)
	if sess.LastCompanyName != "" {
		fmt.Printf(" (%s)", sess.LastCompanyName)
	}
	fmt.Printf("\n     Previous session: %s, %d calls logged\n\n", dateStr, sess.CallsMade)
	fmt.Printf("  r  Resume from #%d\n", sess.CurrentPosition+1)
	fmt.Println("  s  Start over from the beginning")
	fmt.Println("  q  Back to list selection")
	fmt.Println("\n  Press r, s, or q:")
	fmt.Print("> ")

	for {
		key, err := dialer.ReadKey()
		if err != nil {
			return "q"
		}
		switch key {
		case 'r', 'R':
			fmt.Println()
			return "r"
		case 's', 'S':
			fmt.Println()
			return "s"
		case 'q', 'Q', 3:
			fmt.Println()
			return "q"
		}
	}
}

func printResearchBrief(brief research.Brief) {
	if brief.Raw != "" {
		fmt.Println(brief.Raw)
		return
	}
	if brief.WhatTheyDo != "" {
		fmt.Printf("① WHAT THEY DO: %s\n", brief.WhatTheyDo)
	}
	if brief.WhoTheyServe != "" {
		fmt.Printf("② WHO THEY SERVE: %s\n", brief.WhoTheyServe)
	}
	if brief.PaymentComplexity != "" {
		fmt.Printf("③ PAYMENT COMPLEXITY: %s\n", brief.PaymentComplexity)
	}
	if brief.VennFit != "" {
		fmt.Printf("④ VENN FIT: %s\n", brief.VennFit)
	}
	if brief.Hook != "" {
		fmt.Printf("⑤ HOOK: %s\n", brief.Hook)
	}
}

func printQuitSummary(name string, sess *session.SessionState, sessionCalls, sessionStart, currentPos int, dispositions map[string]int, logPath string) {
	fmt.Printf("\n%s\n", strings.Repeat("━", 51))
	fmt.Printf("  SESSION PAUSED — %s\n", name)
	fmt.Printf("%s\n\n", strings.Repeat("━", 51))
	fmt.Printf("  This session:   %d calls  (companies %d–%d)\n", sessionCalls, sessionStart, currentPos)
	if sess != nil && sess.TotalCompanies > 0 {
		pct := int(float64(sess.CurrentPosition) / float64(sess.TotalCompanies) * 100)
		fmt.Printf("  Progress:       %d / %d companies  (%d%%)\n\n", sess.CurrentPosition, sess.TotalCompanies, pct)
	}
	if len(dispositions) > 0 {
		fmt.Println("  Dispositions (this session):")
		printDispositionTable(dispositions)
	}
	fmt.Printf("\n  Session saved. Resume any time.\n\n")
	fmt.Printf("  Log file:\n  %s\n", logPath)
	fmt.Printf("%s\n", strings.Repeat("━", 51))
}

func printCompleteSummary(name string, sess *session.SessionState, sessionCalls, sessionStart, total int, dispositions map[string]int, logPath string) {
	fmt.Printf("\n%s\n", strings.Repeat("━", 51))
	fmt.Printf("  ✓ LIST COMPLETE — %s\n", name)
	fmt.Printf("%s\n\n", strings.Repeat("━", 51))
	fmt.Printf("  This session:   %d calls  (companies %d–%d)\n", sessionCalls, sessionStart, total)
	if sess != nil {
		fmt.Printf("  All sessions:   %d companies total across %d session(s)\n\n", sess.TotalCompanies, sess.SessionCount)
	}
	if len(dispositions) > 0 {
		fmt.Println("  Dispositions (this session):")
		printDispositionTable(dispositions)
	}
	if sess != nil && sess.SessionCount > 1 && len(sess.OutcomeCounts) > 0 {
		fmt.Println("\n  Dispositions (all sessions):")
		printDispositionTable(sess.OutcomeCounts)
	}
	fmt.Printf("\n  Log file:\n  %s\n", logPath)
	fmt.Printf("%s\n", strings.Repeat("━", 51))
}

func printDispositionTable(dispositions map[string]int) {
	order := []string{
		"no_answer",
		"number_not_in_service",
		"contact_retired",
		"instant_hangup_dm",
		"connected_dm",
		"connected_reception",
		"need_to_enrich",
		"other",
	}
	labels := map[string]string{
		"no_answer":             "No Answer",
		"number_not_in_service": "Not in service",
		"contact_retired":       "Contact retired",
		"instant_hangup_dm":     "Hangup w/DM",
		"connected_dm":          "Connected w/DM",
		"connected_reception":   "Connected w/Reception",
		"need_to_enrich":        "Need to enrich",
		"other":                 "Other",
	}
	total := 0
	for _, v := range dispositions {
		total += v
	}
	for _, key := range order {
		count := dispositions[key]
		if count == 0 {
			continue
		}
		pct := 0
		if total > 0 {
			pct = count * 100 / total
		}
		bar := strings.Repeat("█", pct/10) + strings.Repeat("░", 10-pct/10)
		fmt.Printf("  %-22s %3d  %s  %d%%\n", labels[key]+":", count, bar, pct)
	}
}
