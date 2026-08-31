package session

import (
	"strings"
	"testing"
)

func TestParseSession(t *testing.T) {
	line := strings.Join([]string{"work", "3", "1", "1788175300", "1788175999"}, sep)

	got, err := parseSession(line)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if got.Name != "work" {
		t.Errorf("Name = %q, want %q", got.Name, "work")
	}
	if got.Windows != 3 {
		t.Errorf("Windows = %d, want 3", got.Windows)
	}
	if !got.Attached {
		t.Error("Attached = false, want true")
	}
	if got.Created.Unix() != 1788175300 {
		t.Errorf("Created = %d, want 1788175300", got.Created.Unix())
	}
}

// A session name containing pipes, tabs and spaces must survive parsing. tmux
// permits all three, and a separator that collides with them silently shifts
// every later field -- turning a window count into a timestamp rather than
// producing an error anyone would notice.
func TestParseSessionNameWithSeparatorLookalikes(t *testing.T) {
	name := "my|weird\tsession name"
	line := strings.Join([]string{name, "1", "0", "1788175300", "1788175300"}, sep)

	got, err := parseSession(line)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if got.Name != name {
		t.Errorf("Name = %q, want %q", got.Name, name)
	}
	if got.Windows != 1 {
		t.Errorf("Windows = %d, want 1 (fields shifted?)", got.Windows)
	}
	if got.Attached {
		t.Error("Attached = true, want false (fields shifted?)")
	}
}

func TestParseSessionWrongFieldCount(t *testing.T) {
	if _, err := parseSession("only" + sep + "two"); err == nil {
		t.Fatal("expected an error for a short line, got nil")
	}
}

func TestUnixOrZeroOnGarbage(t *testing.T) {
	if got := unixOrZero("not-a-number"); !got.IsZero() {
		t.Errorf("unixOrZero(garbage) = %v, want zero time", got)
	}
}

func TestClientFlags(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		writable bool
		want     string
	}{
		{"fit and writable adds no flags", ModeFit, true, ""},
		{"fit read-only", ModeFit, false, "read-only"},
		{"mirror writable", ModeMirror, true, "ignore-size"},
		{"mirror read-only", ModeMirror, false, "ignore-size,read-only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(clientFlags(tt.mode, tt.writable), ",")
			if got != tt.want {
				t.Errorf("clientFlags = %q, want %q", got, tt.want)
			}
		})
	}
}

// A read-only attach must never omit the read-only flag, whatever the mode.
// This is the one flag that is a security control rather than a preference.
func TestReadOnlyAlwaysPresentWhenNotWritable(t *testing.T) {
	for _, mode := range []Mode{ModeFit, ModeMirror, Mode("bogus"), Mode("")} {
		flags := clientFlags(mode, false)
		found := false
		for _, f := range flags {
			if f == "read-only" {
				found = true
			}
		}
		if !found {
			t.Errorf("mode %q: read-only flag missing from %v", mode, flags)
		}
	}
}
