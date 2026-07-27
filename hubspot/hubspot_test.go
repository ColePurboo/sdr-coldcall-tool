package hubspot

import "testing"

func TestIsPhoneProperty(t *testing.T) {
	cases := []struct {
		name, fieldType, label string
		want                   bool
	}{
		{"mobilephone", "phonenumber", "Mobile Phone Number", true},
		{"phone", "phonenumber", "Phone Number", true},
		{"direct_dial", "string", "Direct Dial", true},      // matched by "direct" hint
		{"cell_number", "string", "Cell Number", true},      // matched by "cell" hint
		{"custom_mobile", "text", "Personal Mobile", true},  // matched by name/label
		{"email", "string", "Email", false},
		{"jobtitle", "string", "Job Title", false},
		{"company", "string", "Company Name", false},
	}
	for _, c := range cases {
		if got := isPhoneProperty(c.name, c.fieldType, c.label); got != c.want {
			t.Errorf("isPhoneProperty(%q,%q,%q) = %v, want %v", c.name, c.fieldType, c.label, got, c.want)
		}
	}
}

func TestPhoneLabelFor(t *testing.T) {
	cases := []struct {
		name, label, want string
	}{
		{"mobilephone", "Mobile Phone Number", "mobile"},
		{"phone", "Phone Number", "office"},
		{"direct_dial", "Direct Dial", "Direct Dial"},
		{"weird_field", "", "weird_field"},
	}
	for _, c := range cases {
		if got := phoneLabelFor(c.name, c.label); got != c.want {
			t.Errorf("phoneLabelFor(%q,%q) = %q, want %q", c.name, c.label, got, c.want)
		}
	}
}

func TestPhoneRank(t *testing.T) {
	if phoneRank("mobilephone") != 0 {
		t.Error("mobilephone should rank 0")
	}
	if phoneRank("phone") != 1 {
		t.Error("phone should rank 1")
	}
	if phoneRank("custom_mobile") != 0 {
		t.Error("a custom mobile field should rank 0")
	}
	if phoneRank("direct_dial") != 2 {
		t.Error("a custom non-mobile field should rank 2")
	}
}
