package dialer

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"sdr-dialer/config"
	"sdr-dialer/leads"
	"sdr-dialer/research"
	"sdr-dialer/session"
)

const barSep = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

// hyperlink wraps text as a terminal OSC 8 hyperlink (clickable in iTerm2/VS Code terminal).
func hyperlink(url, text string) string {
	return fmt.Sprintf("\033]8;;%s\a%s\033]8;;\a", url, text)
}

func websiteLink(website string) string {
	if website == "" {
		return "—"
	}
	fullURL := "https://" + website
	if strings.HasPrefix(website, "http") {
		fullURL = website
	}
	return hyperlink(fullURL, website)
}

// ReadKey reads a single byte in raw terminal mode. Exported for use in main.
func ReadKey() (byte, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		var b [1]byte
		_, readErr := os.Stdin.Read(b[:])
		return b[0], readErr
	}
	defer term.Restore(fd, old)
	var b [1]byte
	_, err = os.Stdin.Read(b[:])
	return b[0], err
}

func readLine() string {
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

func wordWrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var lines []string
	current := ""
	for _, w := range words {
		if current == "" {
			current = w
		} else if len(current)+1+len(w) <= width {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n"+indent)
}

// CallResult holds everything captured during one company interaction.
type CallResult struct {
	Disposition string
	Sentiment   string // only set when Disposition == "connected_dm"
	FreeText    string
	Brief       research.Brief
	Start       time.Time
	End         time.Time
	Skipped     bool
	Quit        bool
	Back        bool // go to previous company
}

// RunCard displays one company card and drives the call interaction.
// researchCh must be a buffered channel (cap 1) with research already in flight.
// hasPrevious indicates whether a previous company exists to go back to.
func RunCard(
	co *leads.Company,
	contactIdx int,
	position, total int,
	csvDisplayName string,
	researchCh chan research.Brief,
	hasPrevious bool,
	cfg *config.Config,
) (CallResult, int) {
	// Separator between company cards (no full clear — SDR can scroll up)
	fmt.Print("\n\n")
	fmt.Println(barSep)
	fmt.Printf("  %s  ·  %d / %d\n", csvDisplayName, position, total)
	fmt.Printf("  %s\n", session.RenderProgressBar(position, total, 30))
	fmt.Println(barSep)

	contact := co.Contacts[contactIdx]
	phone := leads.BestPhone(contact)
	phoneLabel := leads.PhoneLabel(contact)
	if phone == "" {
		phone = "—"
		phoneLabel = "none"
	}

	fmt.Printf("\n  CONTACT   %s %s — %s\n", contact.FirstName, contact.LastName, contact.Title)
	fmt.Printf("  PHONE     %s  (%s)\n", phone, phoneLabel)
	if contact.Email != "" {
		fmt.Printf("  EMAIL     %s\n", contact.Email)
	}
	fmt.Printf("  COMPANY   %s\n", co.Name)

	var parts []string
	if co.City != "" {
		parts = append(parts, co.City)
	}
	if co.Province != "" {
		parts = append(parts, co.Province)
	}
	if co.Employees != "" {
		parts = append(parts, co.Employees+" employees")
	}
	if co.Industry != "" {
		parts = append(parts, co.Industry)
	}
	if len(parts) > 0 {
		fmt.Printf("            %s\n", strings.Join(parts, " · "))
	}
	if co.Website != "" {
		fmt.Printf("  WEBSITE   %s\n", websiteLink(co.Website))
	}

	// Research section — try to show immediately if prefetch already landed
	fmt.Println("\n  ─ RESEARCH ────────────────────────────────────────")
	var brief research.Brief
	briefFetched := false
	select {
	case brief = <-researchCh:
		briefFetched = true
		printBrief(brief)
	default:
		fmt.Println("  ⟳  Researching " + co.Name + "...")
		fmt.Println("     (press ENTER to check again or go straight to call)")
	}
	fmt.Println("  ────────────────────────────────────────────────────")

	// Action prompt
	extra := ""
	if len(co.Contacts) > 1 {
		extra = "    n  Next contact"
	}
	back := ""
	if hasPrevious {
		back = "    b  Back"
	}
	printPrompt := func() {
		fmt.Printf("\n  c  Call    s  Skip%s%s    q  Quit\n\n  Press a key:\n> ", extra, back)
	}
	printPrompt()

	for {
		key, err := ReadKey()
		if err != nil {
			return CallResult{Quit: true}, contactIdx
		}

		switch key {
		case 'c', 'C':
			if !briefFetched {
				select {
				case brief = <-researchCh:
					briefFetched = true
				default:
					select {
					case brief = <-researchCh:
						briefFetched = true
					case <-time.After(200 * time.Millisecond):
						fmt.Println("\n  ⟳  Loading research... (press ENTER to skip)")
						doneCh := make(chan struct{})
						go func() { readLine(); close(doneCh) }()
						select {
						case brief = <-researchCh:
							briefFetched = true
						case <-doneCh:
						}
					}
				}
			}
			result, cidx, aborted := doCall(phone, co, brief, cfg)
			if aborted {
				fmt.Println("\n  ← Call cancelled.")
				printPrompt()
				continue
			}
			result.Brief = brief
			return result, cidx

		case 's', 'S':
			fmt.Println()
			return CallResult{Skipped: true}, contactIdx

		case 'b', 'B':
			if hasPrevious {
				fmt.Println()
				return CallResult{Back: true}, contactIdx
			}

		case 'n', 'N':
			if len(co.Contacts) > 1 {
				next := (contactIdx + 1) % len(co.Contacts)
				var ch chan research.Brief
				if briefFetched {
					ch = make(chan research.Brief, 1)
					ch <- brief
				} else {
					ch = researchCh
				}
				return RunCard(co, next, position, total, csvDisplayName, ch, hasPrevious, cfg)
			}

		case 'q', 'Q', 3:
			return CallResult{Quit: true}, contactIdx

		case '\r', '\n':
			if !briefFetched {
				select {
				case brief = <-researchCh:
					briefFetched = true
					printBrief(brief)
					fmt.Println("  ────────────────────────────────────────────────────")
					printPrompt()
				default:
				}
			}
		}
	}
}

func printBrief(brief research.Brief) {
	if brief.WhatTheyDo == "" && brief.Raw == "" {
		fmt.Println("  (no research available)")
		return
	}
	if brief.Raw != "" {
		fmt.Printf("  %s\n", wordWrap(brief.Raw, 60, "  "))
		return
	}
	if brief.WhatTheyDo != "" {
		fmt.Printf("  ① %s\n", wordWrap(brief.WhatTheyDo, 58, "    "))
	}
	if brief.WhoTheyServe != "" {
		fmt.Printf("  ② %s\n", wordWrap(brief.WhoTheyServe, 58, "    "))
	}
	if brief.PaymentComplexity != "" {
		fmt.Printf("  ③ %s\n", wordWrap(brief.PaymentComplexity, 58, "    "))
	}
	if brief.VennFit != "" {
		fmt.Printf("  ④ %s\n", wordWrap(brief.VennFit, 58, "    "))
	}
	if brief.Hook != "" {
		fmt.Printf("  ⑤ HOOK: %s\n", wordWrap(brief.Hook, 53, "       "))
	}
}

// doCall runs the call flow. Returns (result, contactIdx, aborted).
// aborted=true means the user pressed back mid-flow; no call was logged.
func doCall(phone string, co *leads.Company, brief research.Brief, cfg *config.Config) (CallResult, int, bool) {
	fmt.Printf("\n  ─ CALLING ──────────────────────────────────────────\n")

	callStart := time.Now()

	if cfg != nil && cfg.IsAircallConfigured() && phone != "" && phone != "—" {
		fmt.Printf("  ⟳  Dialing %s via Aircall...\n", phone)
		if err := dialAircall(cfg, phone); err != nil {
			fmt.Printf("\n  ✗  %s\n", err)
			fmt.Println("     Press ENTER to continue anyway, or type 'b' + ENTER to cancel.")
			fmt.Print("> ")
			if line := readLine(); line == "b" || line == "B" {
				return CallResult{}, 0, true
			}
		} else {
			fmt.Println("  ✓  Aircall dial initiated — your softphone should be ringing.")
		}
	} else {
		if phone != "" && phone != "—" {
			fmt.Printf("  ⟳  Dialing %s...\n", phone)
		} else {
			fmt.Printf("  ⟳  Calling %s...\n", co.Name)
		}
		fmt.Println("     (Aircall not configured — simulating call)")
	}

	fmt.Println("\n  ↵  Press ENTER when the call is finished to log the outcome.")
	fmt.Println("  b  Back to card (cancel this call)")
	fmt.Println("  ─────────────────────────────────────────────────────")

	readLine()
	callEnd := time.Now()

	var disposition, sentiment string
	for {
		disposition = promptDisposition()
		if disposition == "" {
			return CallResult{Quit: true}, 0, false
		}
		if disposition == "back" {
			return CallResult{}, 0, true
		}

		if disposition == "connected_dm" {
			sentiment = promptSentiment()
			if sentiment == "back" {
				continue // re-prompt disposition
			}
		}
		break
	}

	fmt.Println("\n  ─ NOTES ────────────────────────────────────────────")
	fmt.Println("  Any notes from the call? (optional)")
	fmt.Println("\n  Type anything and press ENTER, or just press ENTER to skip:")
	fmt.Print("> ")
	freeText := readLine()

	fmt.Println("\n  ─────────────────────────────────────────────────────")
	fmt.Println("  ✓ Call logged.")
	fmt.Println("\n  ↵  Press ENTER to load the next company.")
	fmt.Println("  ─────────────────────────────────────────────────────")
	readLine()

	return CallResult{
		Disposition: disposition,
		Sentiment:   sentiment,
		FreeText:    freeText,
		Brief:       brief,
		Start:       callStart,
		End:         callEnd,
	}, 0, false
}

// promptDisposition returns a disposition key, "" to quit, or "back" to cancel the call.
func promptDisposition() string {
	fmt.Println("\n  ─ DISPOSITION ──────────────────────────────────────")
	fmt.Print("  How did the call go?\n\n")
	fmt.Println("  1  No Answer")
	fmt.Println("  2  Number not in service")
	fmt.Println("  3  Contact retired")
	fmt.Println("  4  Instant hangup w/DM")
	fmt.Println("  5  Connected w/DM")
	fmt.Println("  6  Connected w/Reception")
	fmt.Println("  7  Need to enrich")
	fmt.Println("  8  Other")
	fmt.Println("\n  b  Back to card   q  Quit")
	fmt.Println("\n  Press 1–8:")
	fmt.Print("> ")

	dispositions := map[byte]string{
		'1': "no_answer",
		'2': "number_not_in_service",
		'3': "contact_retired",
		'4': "instant_hangup_dm",
		'5': "connected_dm",
		'6': "connected_reception",
		'7': "need_to_enrich",
		'8': "other",
	}
	for {
		key, err := ReadKey()
		if err != nil || key == 'q' || key == 'Q' || key == 3 {
			return ""
		}
		if key == 'b' || key == 'B' {
			fmt.Println()
			return "back"
		}
		if d, ok := dispositions[key]; ok {
			fmt.Println(string(key))
			return d
		}
	}
}

// promptSentiment returns a sentiment key, or "back" to re-prompt disposition.
// Only called when disposition is "connected_dm".
func promptSentiment() string {
	fmt.Println("\n  ─ SENTIMENT ────────────────────────────────────────")
	fmt.Print("  How did the conversation go?\n\n")
	fmt.Println("  a  Call back later")
	fmt.Println("  b  Pitch - Bad Fit")
	fmt.Println("  c  Pitch - Not Interested")
	fmt.Println("  d  Pitch - 1-2 Months")
	fmt.Println("  e  Pitch - 3-5 Months")
	fmt.Println("  f  Pitch - 6-12 Months")
	fmt.Println("  g  Demo Scheduled")
	fmt.Println("  h  Hang up")
	fmt.Println("  i  Wrong DM Name")
	fmt.Println("  j  DQ this lead")
	fmt.Println("  k  Not the DM")
	fmt.Println("\n  z  Back to disposition")
	fmt.Println("\n  Press a–k:")
	fmt.Print("> ")

	sentiments := map[byte]string{
		'a': "call_back_later",
		'b': "pitch_bad_fit",
		'c': "pitch_not_interested",
		'd': "pitch_1_2_months",
		'e': "pitch_3_5_months",
		'f': "pitch_6_12_months",
		'g': "demo_scheduled",
		'h': "hang_up",
		'i': "wrong_dm_name",
		'j': "dq_lead",
		'k': "not_dm",
	}
	for {
		key, err := ReadKey()
		if err != nil {
			return ""
		}
		if key == 'z' || key == 'Z' {
			fmt.Println()
			return "back"
		}
		if s, ok := sentiments[key]; ok {
			fmt.Println(string(key))
			return s
		}
	}
}
