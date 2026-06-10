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
	Outcome  string
	QuickTag string
	FreeText string
	Brief    research.Brief
	Start    time.Time
	End      time.Time
	Skipped  bool
	Quit     bool
}

// RunCard displays one company card and drives the call interaction.
func RunCard(
	co *leads.Company,
	contactIdx int,
	position, total int,
	csvDisplayName string,
	cfg *config.Config,
	noResearch bool,
) (CallResult, int) {
	// Start research in background
	researchCh := make(chan research.Brief, 1)
	if !noResearch {
		go research.Run(co, cfg, researchCh)
	} else {
		researchCh <- research.Brief{}
	}

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

	// Research section
	fmt.Println("\n  ─ RESEARCH ────────────────────────────────────────")
	var brief research.Brief
	select {
	case brief = <-researchCh:
		printBrief(brief)
	default:
		fmt.Println("  ⟳  Researching " + co.Name + "...")
		fmt.Println("     (press ENTER to skip and go straight to call)")
	}
	fmt.Println("  ────────────────────────────────────────────────────")

	// Action prompt
	extra := ""
	if len(co.Contacts) > 1 {
		extra = "    n  Next contact"
	}
	fmt.Printf("\n  c  Call    s  Skip%s    q  Quit\n\n  Press c, s, n, or q:\n> ", extra)

	for {
		key, err := ReadKey()
		if err != nil {
			return CallResult{Quit: true}, contactIdx
		}

		switch key {
		case 'c', 'C':
			// Wait for research if not yet arrived
			select {
			case brief = <-researchCh:
			default:
				select {
				case brief = <-researchCh:
				case <-time.After(200 * time.Millisecond):
					fmt.Println("\n  ⟳  Loading research... (press ENTER to skip)")
					doneCh := make(chan struct{})
					go func() { readLine(); close(doneCh) }()
					select {
					case brief = <-researchCh:
					case <-doneCh:
					}
				}
			}
			result, cidx := doCall(contact, phone, co, brief)
			result.Brief = brief
			return result, cidx

		case 's', 'S':
			fmt.Println()
			go func() { <-researchCh }()
			return CallResult{Skipped: true}, contactIdx

		case 'n', 'N':
			if len(co.Contacts) > 1 {
				next := (contactIdx + 1) % len(co.Contacts)
				go func() { <-researchCh }()
				return RunCard(co, next, position, total, csvDisplayName, cfg, noResearch)
			}

		case 'q', 'Q', 3:
			go func() { <-researchCh }()
			return CallResult{Quit: true}, contactIdx

		case '\r', '\n':
			select {
			case brief = <-researchCh:
				printBrief(brief)
				fmt.Println("  ────────────────────────────────────────────────────")
			default:
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

func doCall(contact leads.Contact, phone string, co *leads.Company, brief research.Brief) (CallResult, int) {
	fmt.Printf("\n  ─ CALLING ──────────────────────────────────────────\n")
	if phone != "" && phone != "—" {
		fmt.Printf("  ⟳  Dialing %s...\n", phone)
	} else {
		fmt.Printf("  ⟳  Calling %s...\n", co.Name)
	}
	fmt.Println("     (Aircall not configured — simulating call)")
	fmt.Println("\n  ↵  Press ENTER when the call is finished to log the outcome.")
	fmt.Println("  ─────────────────────────────────────────────────────")

	callStart := time.Now()
	readLine()
	callEnd := time.Now()

	outcome := promptOutcome()
	if outcome == "" {
		return CallResult{Quit: true}, 0
	}

	tag := promptTag(outcome)

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
		Outcome:  outcome,
		QuickTag: tag,
		FreeText: freeText,
		Brief:    brief,
		Start:    callStart,
		End:      callEnd,
	}, 0
}

func promptOutcome() string {
	fmt.Println("\n  ─ OUTCOME ──────────────────────────────────────────")
	fmt.Print("  How did the call go?\n\n")
	fmt.Println("  1  Interested")
	fmt.Println("  2  Voicemail")
	fmt.Println("  3  No answer")
	fmt.Println("  4  Not interested")
	fmt.Println("  5  Wrong number / bad data")
	fmt.Println("\n  Press 1–5:")
	fmt.Print("> ")

	outcomes := map[byte]string{
		'1': "interested",
		'2': "voicemail",
		'3': "no_answer",
		'4': "not_interested",
		'5': "wrong_number",
	}
	for {
		key, err := ReadKey()
		if err != nil || key == 'q' || key == 3 {
			return ""
		}
		if o, ok := outcomes[key]; ok {
			fmt.Println(string(key))
			return o
		}
	}
}

var tagOptions = map[string][]string{
	"interested":     {"Send follow-up email", "Book demo", "Send pricing info", "No tag"},
	"voicemail":      {"Left message", "No message — will try again", "No tag"},
	"no_answer":      {"Will try again later", "Try different number", "No tag"},
	"not_interested": {"Has existing solution", "Too small / not a fit", "Wrong person", "No tag"},
	"wrong_number":   {"Number disconnected", "Reached different company", "No tag"},
}

var tagPrompts = map[string]string{
	"interested":     "What's the next step?",
	"voicemail":      "What happened?",
	"no_answer":      "What's the plan?",
	"not_interested": "Why not?",
	"wrong_number":   "What was wrong?",
}

func promptTag(outcome string) string {
	options := tagOptions[outcome]
	if len(options) == 0 {
		return ""
	}
	prompt := tagPrompts[outcome]

	fmt.Println("\n  ─ TAG ──────────────────────────────────────────────")
	fmt.Printf("  %s\n\n", prompt)
	for i, opt := range options {
		fmt.Printf("  %d  %s\n", i+1, opt)
	}
	fmt.Printf("\n  Press 1–%d:\n", len(options))
	fmt.Print("> ")

	for {
		key, err := ReadKey()
		if err != nil {
			return ""
		}
		idx := int(key - '1')
		if idx >= 0 && idx < len(options) {
			selected := options[idx]
			fmt.Println(string(key))
			if selected == "No tag" {
				return ""
			}
			return selected
		}
	}
}
