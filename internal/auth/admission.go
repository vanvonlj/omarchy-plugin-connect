// Package auth decides who may reach the daemon.
//
// This file covers tailnet admission only: a request arriving on the tailnet
// listener, identified by Tailscale rather than by anything the client sent.
// Pairing and device tokens (the tier-2 path) live alongside it later.
package auth

import (
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// Decision is the outcome of an admission check, carrying a reason in both
// directions. A refusal that cannot say why is a support ticket.
type Decision struct {
	Allowed bool

	// Reason is safe to log and to show on the desktop. It is deliberately not
	// returned to the rejected client, which gets a bare 403: telling an
	// unknown caller *why* they failed enumerates the tailnet for them.
	Reason string

	// Peer identifies who was admitted or refused, for the device list and the
	// log. Empty when Tailscale could not identify the caller at all.
	Peer string
}

// Self is the identity of the machine running the daemon.
type Self struct {
	UserID tailcfg.UserID
}

// AdmitTailnet decides whether a tailnet peer may use the daemon.
//
// The rule is "same tailnet user, untagged". Everything else is refused and can
// be paired by hand instead.
func AdmitTailnet(self Self, who *apitype.WhoIsResponse) Decision {
	// Tailscale could not identify the caller. This is the shape of a request
	// that reached the tailnet listener without coming over the tailnet at all,
	// so it is refused before anything else is considered.
	if who == nil || who.Node == nil {
		return Decision{Reason: "no tailnet identity for caller"}
	}

	peer := who.Node.Name
	if peer == "" {
		peer = string(who.Node.StableID)
	}

	// Tagged nodes first, and deliberately before any user comparison.
	//
	// Upstream's own comment on Node.User is the reason: "If ACL tags are in
	// use for the node then it doesn't reflect the ACL identity that the node
	// is running as." A tagged node is owned by an ACL tag, not a person, so
	// "is this the same user as me" has no true answer for it -- but the field
	// still holds *something*, and on some tailnets that something compares
	// equal to the node owner. Checking tags first means a CI runner or a
	// subnet router can never fall through into the allow path.
	//
	// This tailnet has live tagged nodes (tag:k8s), so the case is reachable
	// today rather than theoretical.
	if who.Node.IsTagged() {
		return Decision{
			Reason: "tagged node (" + joinTags(who.Node.Tags) + "); tagged nodes must be paired explicitly",
			Peer:   peer,
		}
	}

	// Untagged and unidentified should not happen -- apitype documents Node and
	// UserProfile as both non-nil in a successful response -- but an absent
	// profile must not be read as "no mismatch, therefore allowed".
	if who.UserProfile == nil {
		return Decision{Reason: "no user profile for caller", Peer: peer}
	}

	// Refuse before comparing if we never learned our own identity. Zero is a
	// valid-looking UserID that compares equal to another zero, so a daemon
	// that failed to read its own status would otherwise admit every caller
	// whose profile was equally unpopulated -- a check that fails open exactly
	// when the thing it depends on is broken.
	if self.UserID == 0 {
		return Decision{Reason: "daemon has not established its own tailnet identity", Peer: peer}
	}

	if who.UserProfile.ID != self.UserID {
		return Decision{
			Reason: "different tailnet user (" + who.UserProfile.LoginName + ")",
			Peer:   peer,
		}
	}

	return Decision{
		Allowed: true,
		Reason:  "same tailnet user (" + who.UserProfile.LoginName + ")",
		Peer:    peer,
	}
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return "untagged"
	}
	out := tags[0]
	for _, t := range tags[1:] {
		out += ", " + t
	}
	return out
}
