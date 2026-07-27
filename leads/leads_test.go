package leads

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"555-123-4567", "+15551234567"},
		{"1 (555) 123-4567", "+15551234567"},
		{"+1 555 123 4567", "+15551234567"},
		{"555.123.4567 ext 22", "+15551234567"},
		{"555-123-4567, 555-999-8888", "+15551234567"}, // first only
		{"", ""},
		{"  ", ""},
		{"12345", "12345"}, // can't normalize -> returned as-is
	}
	for _, c := range cases {
		if got := normalizePhone(c.in); got != c.want {
			t.Errorf("normalizePhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFinanceFirstRank(t *testing.T) {
	cases := []struct {
		title string
		want  int
	}{
		{"CFO", 0},
		{"Chief Financial Officer", 0},
		{"VP Finance", 0},
		{"Director of Financial Planning", 0},
		{"Controller", 0},
		{"Staff Accountant", 0},
		{"Accounts Payable Clerk", 0},
		{"Bookkeeper", 0},
		{"CPA", 0},
		{"Payroll Manager", 0},
		{"CEO", 1},
		{"President & Owner", 1},
		{"Founder", 1},
		{"Head of Marketing", 2},
		{"Account Executive", 2}, // sales, not finance
		{"", 2},
	}
	for _, c := range cases {
		if got := financeFirstRank(c.title); got != c.want {
			t.Errorf("financeFirstRank(%q) = %d, want %d", c.title, got, c.want)
		}
	}
}

func TestSortContactsFinanceFirst(t *testing.T) {
	contacts := []Contact{
		{FirstName: "Ed", Title: "CEO"},
		{FirstName: "Rob", Title: "Head of Sales"},
		{FirstName: "Fran", Title: "CFO"},
		{FirstName: "Alex", Title: "President"},
		{FirstName: "Casey", Title: "Controller"},
	}
	SortContacts(contacts)

	order := []string{}
	for _, c := range contacts {
		order = append(order, c.FirstName)
	}
	// Finance (Fran, Casey) first in original relative order, then execs
	// (Ed, Alex), then the rest (Rob).
	want := []string{"Fran", "Casey", "Ed", "Alex", "Rob"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("sort order = %v, want %v", order, want)
		}
	}
}

func TestDedupePhones(t *testing.T) {
	in := []Phone{
		{Number: "+15551234567", Label: "mobile"},
		{Number: "", Label: "office"},                    // dropped: empty
		{Number: "+15551234567", Label: "direct"},        // dropped: dup, first label wins
		{Number: "+15559998888", Label: "office"},
	}
	got := DedupePhones(in)
	if len(got) != 2 {
		t.Fatalf("DedupePhones len = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].Number != "+15551234567" || got[0].Label != "mobile" {
		t.Errorf("first phone = %+v", got[0])
	}
	if got[1].Number != "+15559998888" || got[1].Label != "office" {
		t.Errorf("second phone = %+v", got[1])
	}
}

func TestPhonesForAndBestPhone(t *testing.T) {
	// Explicit Phones list is the source of truth.
	c := Contact{
		Phones: []Phone{
			{Number: "+15551234567", Label: "mobile"},
			{Number: "+15559998888", Label: "office"},
		},
	}
	if got := BestPhone(c); got != "+15551234567" {
		t.Errorf("BestPhone = %q, want mobile", got)
	}
	if got := PhoneLabel(c); got != "mobile" {
		t.Errorf("PhoneLabel = %q, want mobile", got)
	}

	// Fallback to legacy fields when Phones is empty; office-only -> office.
	c2 := Contact{CompanyPhone: "+15551112222"}
	ph := PhonesFor(c2)
	if len(ph) != 1 || ph[0].Number != "+15551112222" || ph[0].Label != "office" {
		t.Errorf("PhonesFor fallback = %+v", ph)
	}
	if got := PhoneLabel(c2); got != "office" {
		t.Errorf("PhoneLabel(office-only) = %q, want office", got)
	}

	// No numbers at all.
	if got := BestPhone(Contact{}); got != "" {
		t.Errorf("BestPhone(empty) = %q, want empty", got)
	}
	if got := PhoneLabel(Contact{}); got != "none" {
		t.Errorf("PhoneLabel(empty) = %q, want none", got)
	}
}

func TestLoadGroupsAndOrders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.csv")
	csv := "Company Name,First Name,Last Name,Job Title,Email,Company Phone,Mobile Phone\n" +
		"Acme Inc,Ed,Boss,CEO,ed@acme.com,555-100-1000,555-100-2000\n" +
		"acme inc,Fran,Money,CFO,fran@acme.com,555-100-1000,555-100-3000\n" + // same company (case-insensitive)
		"Beta LLC,Sam,Solo,Owner,sam@beta.com,555-200-1000,\n"
	if err := os.WriteFile(path, []byte(csv), 0644); err != nil {
		t.Fatal(err)
	}

	companies, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(companies) != 2 {
		t.Fatalf("got %d companies, want 2", len(companies))
	}

	// Acme: two contacts grouped, CFO sorted ahead of CEO.
	acme := companies[0]
	if acme.Name != "Acme Inc" || len(acme.Contacts) != 2 {
		t.Fatalf("acme = %s with %d contacts", acme.Name, len(acme.Contacts))
	}
	if acme.Contacts[0].Title != "CFO" {
		t.Errorf("acme first contact = %q, want CFO (finance first)", acme.Contacts[0].Title)
	}
	if acme.Primary.Title != "CFO" {
		t.Errorf("acme primary = %q, want CFO", acme.Primary.Title)
	}
	// CFO has both numbers, mobile first.
	cfoPhones := PhonesFor(acme.Contacts[0])
	if len(cfoPhones) != 2 || cfoPhones[0].Label != "mobile" || cfoPhones[0].Number != "+15551003000" {
		t.Errorf("cfo phones = %+v", cfoPhones)
	}

	// Beta: owner has office-only.
	beta := companies[1]
	betaPhones := PhonesFor(beta.Contacts[0])
	if len(betaPhones) != 1 || betaPhones[0].Label != "office" {
		t.Errorf("beta phones = %+v", betaPhones)
	}
}
