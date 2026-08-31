package auth

import (
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const (
	me      = tailcfg.UserID(2365911222796709)
	someone = tailcfg.UserID(1111111111111111)
)

func who(name string, user tailcfg.UserID, tags ...string) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node: &tailcfg.Node{Name: name, User: user, Tags: tags},
		UserProfile: &tailcfg.UserProfile{
			ID:        user,
			LoginName: "luke-serv@outlook.com",
		},
	}
}

func TestAdmitTailnet(t *testing.T) {
	self := Self{UserID: me}

	tests := []struct {
		name string
		who  *apitype.WhoIsResponse
		want bool
	}{
		{
			name: "same user, untagged, is admitted",
			who:  who("omarchy-hp.tail18c58.ts.net", me),
			want: true,
		},
		{
			name: "different tailnet user is refused",
			who:  who("someone-else.tail18c58.ts.net", someone),
			want: false,
		},
		{
			name: "unidentified caller is refused",
			who:  nil,
			want: false,
		},
		{
			name: "response with no node is refused",
			who:  &apitype.WhoIsResponse{},
			want: false,
		},
		{
			name: "untagged caller with no user profile is refused",
			who: &apitype.WhoIsResponse{
				Node: &tailcfg.Node{Name: "odd.tail18c58.ts.net", User: me},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AdmitTailnet(self, tt.who)
			if got.Allowed != tt.want {
				t.Errorf("Allowed = %v, want %v (reason: %s)", got.Allowed, tt.want, got.Reason)
			}
			if got.Reason == "" {
				t.Error("Decision carries no reason; refusals and admissions must both be explicable")
			}
		})
	}
}

// A tagged node must be refused even when its Node.User matches ours exactly.
//
// This is the whole reason tags are checked before users. tailcfg documents
// Node.User as not reflecting the ACL identity of a tagged node, so the field
// can hold the tailnet owner's ID while the node is really a CI runner or a
// subnet router. A "same user" comparison alone would hand it a shell.
func TestTaggedNodeRefusedEvenWhenUserMatches(t *testing.T) {
	self := Self{UserID: me}

	tagged := who("local-network-connector.tail18c58.ts.net", me, "tag:k8s")
	got := AdmitTailnet(self, tagged)

	if got.Allowed {
		t.Fatal("tagged node was admitted; tags must be checked before the user comparison")
	}
	if got.Peer != "local-network-connector.tail18c58.ts.net" {
		t.Errorf("Peer = %q, want the node name so the refusal is actionable", got.Peer)
	}
}

// A tagged node with no user profile at all is the shape tailscaled actually
// returns for tag-owned machines. It must refuse for the tag reason, not fall
// through to the nil-profile branch, so the log says something useful.
func TestTaggedNodeWithoutUserProfile(t *testing.T) {
	got := AdmitTailnet(Self{UserID: me}, &apitype.WhoIsResponse{
		Node: &tailcfg.Node{
			Name: "tailscale-operator.tail18c58.ts.net",
			Tags: []string{"tag:k8s"},
		},
	})

	if got.Allowed {
		t.Fatal("tagged node without a user profile was admitted")
	}
	if want := "tagged node (tag:k8s); tagged nodes must be paired explicitly"; got.Reason != want {
		t.Errorf("Reason = %q, want %q", got.Reason, want)
	}
}

// The zero Self must not admit a node whose user is also zero. An uninitialised
// identity comparing equal to an uninitialised caller is the classic way this
// kind of check fails open.
func TestZeroSelfDoesNotAdmitZeroUser(t *testing.T) {
	got := AdmitTailnet(Self{}, &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: "unknown"},
		UserProfile: &tailcfg.UserProfile{},
	})
	if got.Allowed {
		t.Fatal("zero-value identity admitted a zero-value caller; the check fails open")
	}
}
