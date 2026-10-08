package command

import (
	"slices"
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	tests := map[string]struct {
		line string
		want []string
	}{
		"plain":            {"set system host-name edge", []string{"system", "host-name", "edge"}},
		"valueless":        {"set service https api rest", []string{"service", "https", "api", "rest"}},
		"single quoted":    {"set interfaces ethernet eth0 description 'WAN - Internet'", []string{"interfaces", "ethernet", "eth0", "description", "WAN - Internet"}},
		"double quoted":    {`set system login banner pre-login "hello \"you\""`, []string{"system", "login", "banner", "pre-login", `hello "you"`}},
		"empty value":      {"set system login user vyos authentication plaintext-password ''", []string{"system", "login", "user", "vyos", "authentication", "plaintext-password", ""}},
		"escaped quote":    {`set a b 'it'\''s'`, []string{"a", "b", "it's"}},
		"extra whitespace": {"  set   a \t b  ", []string{"a", "b"}},
		"dollar in quotes": {"set a b '$6$salt$hash'", []string{"a", "b", "$6$salt$hash"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseLine(tt.line)
			if err != nil {
				t.Fatalf("ParseLine: %v", err)
			}
			if !slices.Equal([]string(got), tt.want) {
				t.Errorf("got %q, want %q", []string(got), tt.want)
			}
		})
	}
}

func TestParseLineRejects(t *testing.T) {
	tests := map[string]string{
		// The configuration is declarative: what should not be there is
		// simply not listed.
		"delete":             "delete system host-name",
		"not a command":      "system host-name edge",
		"set alone":          "set",
		"unterminated":       "set a b 'c",
		"unterminated dq":    `set a b "c`,
		"trailing backslash": `set a b c\`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := ParseLine(line); err == nil {
				t.Errorf("ParseLine(%q) = %q, want an error", line, []string(got))
			}
		})
	}
}

func TestParse(t *testing.T) {
	text := `
# WAN
set interfaces ethernet eth0 address dhcp

   # indented comment
set system host-name edge
set interfaces ethernet eth0 address dhcp
`
	got, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Order is kept, a repeated line is listed once.
	want := []string{"set interfaces ethernet eth0 address dhcp", "set system host-name edge"}
	if lines := Lines(got); !slices.Equal(lines, want) {
		t.Errorf("got %q, want %q", lines, want)
	}
}

func TestParseReportsTheLine(t *testing.T) {
	_, err := Parse("set a b\n\nbogus line\n")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want it to name line 3", err)
	}
}

func TestStringRoundTrips(t *testing.T) {
	// String is how a command is written back, and what the hash of a
	// configuration is computed over, so it has to parse to the same path.
	for _, path := range []Path{
		{"system", "host-name", "edge"},
		{"service", "https", "api", "rest"},
		{"a", "b", "it's"},
		{"a", "b", ""},
		{"a", "b", "two words"},
	} {
		parsed, err := ParseLine(path.String())
		if err != nil {
			t.Errorf("%q: %v", path.String(), err)
			continue
		}
		if !slices.Equal(parsed, path) {
			t.Errorf("%q parsed to %q, want %q", path.String(), []string(parsed), []string(path))
		}
	}
}

func TestHashIgnoresOrder(t *testing.T) {
	a, _ := Parse("set a b\nset c d\n")
	b, _ := Parse("set c d\nset a b\n")
	c, _ := Parse("set c d\nset a x\n")
	if Hash(a) != Hash(b) {
		t.Error("the same commands in another order hash differently")
	}
	if Hash(a) == Hash(c) {
		t.Error("different commands hash the same")
	}
	if Hash(nil) == "" {
		t.Error("Hash(nil) is empty")
	}
}
