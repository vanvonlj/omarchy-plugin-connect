package transport

import "testing"

// Real lines from /etc/ufw/user.rules on this machine, which is the only
// format that matters: a hand-written approximation would pass while the file
// the daemon actually reads did not.
const sampleRules = `*filter
:ufw-user-input - [0:0]
### tuple ### allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in_wlp1s0
-A ufw-user-input -i wlp1s0 -p tcp --dport 22 -j ACCEPT
### tuple ### allow tcp 7433 0.0.0.0/0 any 0.0.0.0/0 in_wlp1s0
-A ufw-user-input -i wlp1s0 -p tcp --dport 7433 -j ACCEPT
COMMIT
`

func TestRulesAllow(t *testing.T) {
	tests := []struct {
		name  string
		port  int
		iface string
		want  bool
	}{
		{"the rule that was added", 7433, "wlp1s0", true},
		{"a port with no rule", 9999, "wlp1s0", false},
		{"right port, wrong interface", 7433, "eth0", false},
		{"a different allowed port", 22, "wlp1s0", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rulesAllow(sampleRules, tt.port, tt.iface); got != tt.want {
				t.Errorf("rulesAllow(%d, %q) = %v, want %v", tt.port, tt.iface, got, tt.want)
			}
		})
	}
}

// A rule with no -i applies to every interface, so it covers us too. Reading it
// as "not our interface" would nag someone who has already opened the port.
func TestRulesAllowInterfaceAgnosticRule(t *testing.T) {
	rules := "-A ufw-user-input -p tcp --dport 7433 -j ACCEPT\n"
	if !rulesAllow(rules, 7433, "wlp1s0") {
		t.Fatal("a rule with no interface did not count as allowing the port")
	}
}

// A port appearing in a DROP or in a comment is not permission. Matching the
// number alone would silence the warning on a machine that is still blocking.
func TestRulesAllowIgnoresNonAccept(t *testing.T) {
	rules := "### tuple ### deny tcp 7433 0.0.0.0/0 any 0.0.0.0/0 in_wlp1s0\n" +
		"-A ufw-user-input -i wlp1s0 -p tcp --dport 7433 -j DROP\n"
	if rulesAllow(rules, 7433, "wlp1s0") {
		t.Fatal("a DROP rule was read as allowing the port")
	}
}

// Substring matching on the port number would let 74330 satisfy a check for
// 7433, which is the classic way this kind of parse goes wrong.
func TestRulesAllowDoesNotMatchPortPrefix(t *testing.T) {
	rules := "-A ufw-user-input -i wlp1s0 -p tcp --dport 74330 -j ACCEPT\n"
	if rulesAllow(rules, 7433, "wlp1s0") {
		t.Fatal("port 74330 satisfied a check for 7433")
	}
}
