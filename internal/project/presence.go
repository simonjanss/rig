package project

import (
	"reflect"
	"time"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Presence is who is here, and what they are looking at.
//
// It is configuration rather than a Go literal for the reason `auth:`, `files:`
// and `notifications:` are: a TTL is the number that decides how long a closed
// laptop goes on looking like somebody editing, and a number like that written in
// a main function is one the generated documentation cannot quote and nobody can
// find without reading the wiring.
//
// Two of these values are also **answered to the browser**. The heartbeat
// response carries the TTL and the heartbeat interval, so changing either is a
// deploy of the server rather than a release of the front end — which is the
// whole reason they are not constants in the browser package.
//
// Zero means "left out" throughout, and there is deliberately no way to write a
// zero that survives: a TTL of nothing is not a configuration anybody wants.
type Presence struct {
	// Enabled says this project tracks presence. Off by default, and what makes
	// `server-go` write the wiring and the compiler keep rig_presence in the
	// schema at all.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether this project tracks presence. The server-go generator writes the wiring only when it is set."`

	// Expose projects rig_presence as a resource, so presence gets the filter
	// grammar, the sort keys and a generated client.
	//
	// Off is the ordinary case and still leaves presence working: the routes
	// under /presence are hand-written, because the table is the same in every
	// project and there is nothing for a generator to vary, and the live shape is
	// how a browser actually reads it. This is for the project those are not
	// enough for.
	//
	// Read-only either way. The scaffolded table configuration is
	// `operations: [Get, List]` and no more: a client that could POST one of
	// these could claim to be somebody else, and the heartbeat route exists
	// precisely so that it cannot.
	Expose bool `yaml:"expose,omitempty" json:"expose,omitempty" jsonschema_description:"Project presence as a generated read-only resource as well, for the filter grammar and a typed client. The hand-written routes and the live shape exist either way."`

	// TTL is how long a session stays present after its last heartbeat.
	//
	// It is the cost of the whole feature stated as one number. Short, and
	// somebody on hotel wifi flickers in and out of everybody else's view; long,
	// and a closed laptop goes on editing the title for that long.
	//
	// The long end is survivable because this is the *backstop*: an ordinary
	// close, a navigation or a tab going to the background sends a leave, so the
	// TTL only has to cover a crashed tab and a dead network. That is what makes
	// a minute affordable rather than five seconds.
	TTL Duration `yaml:"ttl,omitempty" json:"ttl,omitempty" jsonschema_description:"How long a session stays present after its last heartbeat. Defaults to 1m; under 15s is refused, and it has to be at least three times heartbeat."`

	// Heartbeat is how often a browser says it is still there, and it is
	// answered to the browser rather than configured in it.
	//
	// Every beat is one row changed, which every subscriber to the presence
	// shape hears about. So this is not a latency knob: it is the write rate and
	// the fan-out rate, multiplied by tabs. See docs/presence.md for the
	// arithmetic before lowering it.
	Heartbeat Duration `yaml:"heartbeat,omitempty" json:"heartbeat,omitempty" jsonschema_description:"How often a browser confirms it is still there. Defaults to 20s. Every beat is one row change fanned out to every subscriber, so this is the write rate rather than a latency knob."`

	// Sweep is how often the in-process sweeper deletes rows past their window.
	//
	// It is not what makes somebody disappear — whoever is reading decides that,
	// against the clock, and within a second. This is what keeps the table, and
	// every new subscriber's first fetch, from growing forever.
	//
	// **There is no value here that turns the sweeper off**, and there was going
	// to be: a negative duration cannot be written in this file, because
	// [DurationPattern] does not admit a sign. That turned out to be the right
	// answer rather than a limitation to work around. Whether a process runs a
	// background loop is a line in its own main function, not a number in a
	// configuration file — the `notifications:` block has no key for it either,
	// and an operator who wants the cron job to own this simply does not call
	// Start on the sweeper. This key is how often it ticks when it is running.
	Sweep Duration `yaml:"sweep,omitempty" json:"sweep,omitempty" jsonschema_description:"How often the in-process sweeper ticks when it is running. Defaults to 1m. Whether it runs at all is a line in main.go, not a value here."`

	// Grace is how long past the TTL a row is kept before it is deleted.
	//
	// It exists so the two expiry mechanisms cannot disagree. A subscriber stops
	// drawing a row at the TTL and the sweeper deletes it at TTL plus this, so a
	// row is always invisible before it is gone — never the other way round,
	// which would be a row that came back when a slow client caught up.
	Grace Duration `yaml:"grace,omitempty" json:"grace,omitempty" jsonschema_description:"How long past the TTL a row survives before the sweeper removes it. Defaults to 5m. It is what keeps a row from being deleted while a subscriber is still deciding about it."`
}

// MinPresenceTTL is the shortest window rig will start with.
//
// Under fifteen seconds presence flickers on an ordinary mobile connection, and
// what that presents as is a broken feature rather than a number somebody chose.
// The presence module refuses the same value at construction; this is the earlier
// and cheaper place to notice.
const MinPresenceTTL = 15 * time.Second

// MinPresenceHeartbeat is the shortest heartbeat the document can carry, which is
// the only reason there is one: every duration in this block is resolved to whole
// seconds for the IR, so anything under a second arrives as zero and zero reads
// as unset.
const MinPresenceHeartbeat = time.Second

// PresenceBeatsBeforeGone is how many heartbeats have to be lost before a session
// disappears, and therefore the ratio the TTL is checked against.
//
// Three, and each one is a different failure: one for a garbage-collection pause,
// one for a slow network, one for the request that was actually lost. At two, an
// ordinary hiccup makes somebody vanish from everybody else's screen.
const PresenceBeatsBeforeGone = 3

// Configured reports whether the presence block says anything beyond whether
// there is presence and how its table is treated, the same way
// [Notifications.Configured] does.
func (p Presence) Configured() bool {
	bare := Presence{Enabled: p.Enabled, Expose: p.Expose}
	return !reflect.DeepEqual(p, bare)
}

// checkPresence validates what the JSON Schema cannot: values that only make
// sense relative to each other, and a block nothing reads.
func (p *Project) checkPresence() diag.List {
	var diags diag.List
	c := p.Config.Presence

	if !c.Enabled {
		// The same failure mode the auth, files and notifications blocks have,
		// and refused for the same reason: a TTL somebody set and believed in,
		// unread.
		if c.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("presence", "enabled"),
				"presence is configured but presence.enabled is false, so none of it is "+
					"read; set `enabled: true` or remove the block")
		}
		return diags
	}

	if ttl := c.TTL.Duration(); ttl > 0 && ttl < MinPresenceTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("presence", "ttl"),
			"presence.ttl is %s, and under %s a session flickers in and out of everybody "+
				"else's view on an ordinary mobile connection — which presents as a broken "+
				"feature rather than as a number somebody chose",
			c.TTL, MinPresenceTTL)
	}

	// A floor, because the document carries this in seconds. `500ms` resolves to
	// zero, the generated wiring passes zero, and the presence module reads zero
	// as "unset" and uses twenty seconds — forty times what was asked for,
	// quietly. Refused rather than rounded, because the two are indistinguishable
	// afterwards.
	if beat := c.Heartbeat.Duration(); beat > 0 && beat < MinPresenceHeartbeat {
		diags.Add(diag.CodeConfigInvalid, p.At("presence", "heartbeat"),
			"presence.heartbeat is %s, and under %s cannot be carried: the value is "+
				"resolved in seconds and would arrive as none at all, which reads as unset "+
				"and becomes the %s default",
			c.Heartbeat, MinPresenceHeartbeat, DefaultPresenceHeartbeat)
	}

	// The one misconfiguration worth understanding before deploying this, and it
	// is stated as arithmetic rather than as advice: both values are fine alone,
	// which is what makes this a check on the pair.
	if ttl, beat := c.TTL.Duration(), c.Heartbeat.Duration(); ttl > 0 && beat > 0 &&
		ttl < PresenceBeatsBeforeGone*beat {
		diags.Add(diag.CodeConfigInvalid, p.At("presence", "ttl"),
			"presence.heartbeat is %s and presence.ttl is %s, so a session vanishes from "+
				"everybody else's view after one lost request. %d beats is the floor: one "+
				"for a pause, one for a slow network, one for the request that was actually "+
				"lost — so ttl has to be at least %s",
			c.Heartbeat, c.TTL, PresenceBeatsBeforeGone,
			Duration(PresenceBeatsBeforeGone*beat))
	}

	// A warning rather than a refusal: it works, it just spends DELETEs deleting
	// rows every subscriber had already stopped drawing.
	if sweep, ttl := c.Sweep.Duration(), c.TTL.Duration(); sweep > 0 && ttl > 0 && sweep < ttl {
		diags.AddSeverity(diag.CodeConfigInvalid, diag.SeverityWarning, p.At("presence", "sweep"),
			"presence.sweep is %s and presence.ttl is %s, so the sweeper runs more often "+
				"than a row can expire. It is not wrong — it is a pass that mostly finds "+
				"nothing, on the table with the most write traffic in the application",
			c.Sweep, c.TTL)
	}

	if grace := c.Grace.Duration(); grace < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("presence", "grace"),
			"presence.grace is %s; a negative grace would delete a row before the last "+
				"subscriber stopped drawing it, which is a row that comes back", c.Grace)
	}

	// What presence needs is the tenancy tables, not the `auth:` block: a
	// presence row names an account, and an application whose claims come from a
	// header has accounts like any other. Whether those tables are there is a
	// question about migrations, and it is asked where the migrations are.

	return diags
}

// IR is the resolved block, as a document carries it.
//
// Nil for a project without presence, so that a generator asks the document one
// question — is there presence — rather than reading a flag and then four numbers
// that may or may not mean anything.
//
// Every duration is resolved to seconds here, for the reason
// [Notifications.IR] gives.
func (p Presence) IR() *ir.Presence {
	if !p.Enabled {
		return nil
	}

	return &ir.Presence{
		Enabled:          true,
		Expose:           p.Expose,
		TTLSeconds:       int64(p.TTL.Duration().Seconds()),
		HeartbeatSeconds: int64(p.Heartbeat.Duration().Seconds()),
		SweepSeconds:     int64(p.Sweep.Duration().Seconds()),
		GraceSeconds:     int64(p.Grace.Duration().Seconds()),
	}
}
