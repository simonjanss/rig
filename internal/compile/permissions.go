package compile

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/ir"
)

// The three actions every resource has, derived from what it exposes.
//
// Three rather than one per operation. Create and Update as separate keys reads
// as more precise and is not: no role design anybody writes distinguishes them,
// and every extra key is another row to grant and another thing to explain.
const (
	actionRead   = "read"
	actionWrite  = "write"
	actionDelete = "delete"
)

// The values of a table's `access.scope`, and of the reserved request parameter
// that widens it.
//
// The parameter's own vocabulary lives in [tenancy.Scope], where the handler and
// the client both read it. These are here because the compiler has to name them
// in a diagnostic before any of that exists.
const (
	accessScopeTenant = "tenant"
	accessScopeOwn    = "own"
	accessScopeAll    = "all"
)

// wideReadPermission is what a caller must hold to ask an owner-scoped resource
// for the whole tenant.
//
// A third segment rather than a separate key like "note.read_all", so the
// relationship is legible in a list of grants: everything under note.read is
// reading notes, and the suffix says how far. A screen that groups by prefix gets
// the grouping for free.
func wideReadPermission(table string) string {
	return table + "." + actionRead + "." + accessScopeAll
}

// computePermissions fills in every endpoint's permission and collects the
// catalogue.
//
// Derived here, once, for the reason [ir.Endpoint.Pattern] is: the check the
// handler makes, the rows written to the permission table, the grant an owner
// receives and whatever an administration screen lists all have to agree, and a
// value each of them derives separately is a value that eventually disagrees.
//
// A permission a table's configuration declared wins. Nothing else is invented:
// an endpoint that is public gets none, because being open is the thing that had
// to be written down.
func computePermissions(doc *ir.Document, mode project.PermissionMode) diag.List {
	var diags diag.List

	if mode == project.PermissionsNone {
		// A project with no authorization at all. `public:` still works, and
		// nothing here is derived — an endpoint's declared permission is left
		// alone, so a project can turn the default off and still ask for a check
		// on the one endpoint that needs one.
		//
		// The widening key goes, though, because it was never declared: it was
		// derived from the table asking to be owner-scoped, and a key nothing
		// catalogues would refuse every ?scope=all in a project that asked for no
		// authorization. With it empty the parameter still narrows by default and
		// still widens on request — which is what a project with no permissions
		// should get.
		for i := range doc.API.Resources {
			for j := range doc.API.Resources[i].Endpoints {
				doc.API.Resources[i].Endpoints[j].WidePermission = ""
			}
		}
		return diags
	}

	seen := map[string]ir.Permission{}
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]

		for j := range res.Endpoints {
			ep := &res.Endpoints[j]
			if ep.Public {
				// Declared open. A permission here would be dead weight, and
				// leaving it empty is what makes `public: true` mean something.
				ep.Permission = ""
				continue
			}

			action := actionFor(ep)
			if ep.Permission == "" {
				ep.Permission = res.Storage.Table + "." + action
			}

			p := ir.Permission{
				Key:         ep.Permission,
				Name:        permissionName(res, ep, action),
				Description: permissionDoc(res, ep, action),
				Resource:    res.Name,
				Action:      action,
			}
			// Several endpoints share one key — Get, List and Search are all
			// reading — and the first to define it wins, so the description
			// describes the action rather than whichever endpoint sorted first.
			if _, ok := seen[p.Key]; !ok {
				seen[p.Key] = p
			}

			// The widening key is catalogued from the endpoint that offers it, so
			// it exists in the permission table exactly when something checks it.
			// A grantable permission nothing enforces is worse than a missing one:
			// somebody hands it out and believes it did something.
			if ep.WidePermission != "" {
				if _, ok := seen[ep.WidePermission]; !ok {
					seen[ep.WidePermission] = ir.Permission{
						Key:  ep.WidePermission,
						Name: "Read all " + strings.ToLower(res.Plural),
						Description: "Read every " + strings.ToLower(res.Name) +
							" in the tenant, not only the caller's own. Held in addition to " +
							res.Storage.Table + "." + actionRead + " rather than instead of it: " +
							"the endpoint's own check runs first, so this one widens a read " +
							"somebody may already do and grants nothing on its own. Without it, " +
							"?" + ir.ScopeParam + "=" + accessScopeAll + " is refused.",
						Resource: res.Name,
						Action:   actionRead + "." + accessScopeAll,
					}
				}
			}
		}
	}

	doc.API.Permissions = make([]ir.Permission, 0, len(seen))
	for _, p := range seen {
		doc.API.Permissions = append(doc.API.Permissions, p)
	}
	slices.SortFunc(doc.API.Permissions, func(a, b ir.Permission) int {
		return cmp.Compare(a.Key, b.Key)
	})

	return diags
}

// actionFor is what an endpoint does, in permission terms.
//
// A custom endpoint gets its own action rather than being folded into write: a
// role that may publish a lesson is not the same as a role that may edit one, and
// that difference is the reason somebody wrote a custom endpoint in the first
// place.
func actionFor(ep *ir.Endpoint) string {
	// A file column gets two keys of its own, so that "may edit a profile" and
	// "may replace its picture" are separable grants.
	//
	// Nothing implies them. Folding the write into the resource's own write key
	// would make the common case one grant, and it would mean a role quietly
	// gaining the ability to replace an image the day somebody adds a file
	// column to a table it could already edit. A permission that is implied is a
	// permission nobody audits. The price is two more rows per file column in
	// every grant table, and a role that may edit a profile and cannot touch its
	// picture until someone says so.
	//
	// Two rather than three: the delete is a write, because detaching a file and
	// replacing it are the same decision, and a role trusted to swap somebody's
	// picture is not meaningfully restrained by being unable to remove it.
	if ep.File != nil {
		stem := strings.TrimSuffix(ep.File.Column, "_id")
		if ep.Method == http.MethodGet {
			return stem + "." + actionRead
		}
		return stem + "." + actionWrite
	}

	switch ep.Name {
	case ir.OpGet, ir.OpList, ir.OpSearch, ir.OpListDeleted, ir.OpVersions:
		return actionRead
	case ir.OpCreate, ir.OpUpdate, ir.OpRestore, ir.OpRevert:
		return actionWrite
	case ir.OpDelete:
		return actionDelete
	default:
		return strings.ToLower(ep.Name)
	}
}

// permissionName is what an administration screen shows.
func permissionName(res *ir.Resource, ep *ir.Endpoint, action string) string {
	if ep.File != nil {
		verb := "Replace"
		if strings.HasSuffix(action, "."+actionRead) {
			verb = "Read"
		}
		return verb + " the " + humanRole(*ep.File) + " of " + strings.ToLower(res.Plural)
	}

	switch action {
	case actionRead:
		return "Read " + strings.ToLower(res.Plural)
	case actionWrite:
		return "Write " + strings.ToLower(res.Plural)
	case actionDelete:
		return "Delete " + strings.ToLower(res.Plural)
	default:
		return strings.ToUpper(action[:1]) + action[1:] + " " + strings.ToLower(res.Name)
	}
}

// permissionDoc explains what holding it lets somebody do.
//
// It leans on the resource's own description, which came from the table's
// COMMENT ON — so the sentence a person reads when granting a permission is the
// sentence somebody already wrote about the table, rather than a second copy of
// it that drifts.
func permissionDoc(res *ir.Resource, ep *ir.Endpoint, action string) string {
	subject := strings.ToLower(res.Plural)
	if res.Description != "" {
		subject += " — " + strings.TrimSuffix(res.Description, ".")
	}

	if ep.File != nil {
		role := humanRole(*ep.File)
		if strings.HasSuffix(action, "."+actionRead) {
			return "Download the " + role + " of " + subject + "."
		}
		return "Attach, replace and remove the " + role + " of " + subject + ". " +
			"Held in addition to " + res.Storage.Table + "." + actionWrite + " rather than " +
			"implied by it: a role that may edit the row is not automatically a role " +
			"that may change its " + role + "."
	}

	switch action {
	case actionRead:
		return "Read " + subject + "."
	case actionWrite:
		return "Create and change " + subject + "."
	case actionDelete:
		return "Delete " + subject + "."
	default:
		if ep.Summary != "" {
			return ep.Summary
		}
		return ep.Name + " on " + strings.ToLower(res.Name) + "."
	}
}
