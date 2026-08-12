package compile

import "github.com/simonjanss/rig/pkg/ir"

// Suffixes of the generated filter shapes.
const (
	suffixFilter         = "Filter"
	suffixFilterEquals   = "FilterEquals"
	suffixFilterRange    = "FilterRange"
	suffixFilterContains = "FilterContains"
	suffixFilterLike     = "FilterLike"
	suffixFilterNull     = "FilterNull"
	suffixFilterWithout  = "FilterWithout"
)

// filterObjects builds the shapes a Search request body is made of.
//
// Splitting the operators into separate objects rather than tagging each field
// with a comparator is what keeps the whole thing typed: FilterRange only
// carries fields that can be ordered, FilterLike only strings, so asking for
// "created_at contains 3" is not expressible rather than merely rejected.
//
// Every field is optional. A filter object with nothing set matches everything,
// which makes Search with an empty body the same as List.
func filterObjects(res ir.Resource, readable []ir.Field, wire func(string) string, exposed map[string]bool) []ir.Object {
	base := res.Name

	var (
		equals   []ir.Field
		ranged   []ir.Field
		contains []ir.Field
		like     []ir.Field
		null     []ir.Field
	)

	for _, f := range readable {
		// A relation embedded into the response is not a column, so there is
		// nothing to filter it by here.
		if f.Column == nil {
			continue
		}
		// The tenant is not a condition anybody gets to write. Every read is
		// already scoped to the caller's, ANDed above whatever they sent, so a
		// condition here could only ever be a no-op or a contradiction — ask
		// for your own tenant and nothing changes, ask for another and you get
		// an empty page. Offering the field would document a filter that does
		// not filter, and invite somebody to think it might.
		if s := res.Storage; s != nil && s.Tenant != nil && s.Tenant.Name == f.Column.Name {
			continue
		}

		equals = append(equals, optional(f, wire))

		if isComparable(f) {
			ranged = append(ranged, optional(f, wire))
		}
		if !f.IsArray() {
			contains = append(contains, asArray(f, wire))
		}
		if f.IsTextual() {
			like = append(like, optional(f, wire))
		}
		if f.IsNullable() {
			null = append(null, asBool(f, wire))
		}
	}

	// A relation is a condition of the same kind as the ones around it, so it
	// sits beside them: FilterEquals carries the far side's FilterEquals, and a
	// condition on a related row is written where a condition on this row
	// would be.
	rel := func(suffix string) []ir.Field {
		return relationFilterFields(res, wire, exposed, suffix, relationFilterDoc)
	}

	// The absence question, which no operator on a related row can ask: every
	// one of those is satisfied by some related row, and "none" is satisfied by
	// there being nothing to look at.
	//
	// It carries the far side's whole filter rather than one operator's object,
	// because it is a single condition about the far side and negation is where
	// the difference between "and" and "or" starts to matter.
	without := relationFilterFields(res, wire, exposed, suffixFilter, relationAbsenceDoc)

	objs := []ir.Object{
		{
			Name:        base + suffixFilterEquals,
			Description: "Exact-match conditions on " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      append(equals, rel(suffixFilterEquals)...),
		},
		{
			Name:        base + suffixFilterRange,
			Description: "Ordering conditions on " + res.Name + " fields that can be compared.",
			Origin:      ir.OriginFilter,
			Fields:      append(ranged, rel(suffixFilterRange)...),
		},
		{
			Name:        base + suffixFilterContains,
			Description: "Set-membership conditions on " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      append(contains, rel(suffixFilterContains)...),
		},
		{
			Name:        base + suffixFilterLike,
			Description: "Pattern conditions on " + res.Name + " text fields.",
			Origin:      ir.OriginFilter,
			Fields:      append(like, rel(suffixFilterLike)...),
		},
		{
			Name:        base + suffixFilterNull,
			Description: "Presence conditions on nullable " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      append(null, rel(suffixFilterNull)...),
		},
		{
			Name:        base + suffixFilter,
			Description: "Conditions a " + res.Name + " must satisfy to match a search.",
			Origin:      ir.OriginFilter,
			Fields:      filterFields(base, wire),
		},
	}

	// A table with no relations has nothing to be without, and an object with no
	// fields is a condition nobody can write.
	if len(without) > 0 {
		objs = append(objs, ir.Object{
			Name:        base + suffixFilterWithout,
			Description: "Relations a " + res.Name + " must have no matching row for.",
			Origin:      ir.OriginFilter,
			Fields:      without,
		})

		last := len(objs) - 2 // the filter itself, before the object just added
		objs[last].Fields = append(objs[last].Fields, ir.Field{
			Name: "Without", Wire: wire("Without"),
			Type:      base + suffixFilterWithout,
			TypeKind:  ir.TypeKindObject,
			GoType:    "*" + base + suffixFilterWithout,
			Modifiers: []string{ir.ModifierNullable},
			Description: "Relations with no row matching the given conditions. An empty " +
				"one asks for no related row at all.\n\n" +
				"This is where a negation belongs when what you mean is \"not\": it " +
				"negates whether a matching related row exists, so a row with no " +
				"related row at all satisfies it. The negative operators above negate " +
				"the comparison instead and still require the related row to be " +
				"there — \"no related row called X\" is here, \"a related row not " +
				"called X\" is NotEquals.",
		})
	}

	return objs
}

// filterFields is the top-level filter: one entry per operator, plus the two
// controls that turn a flat set of conditions into a boolean expression.
func filterFields(base string, wire func(string) string) []ir.Field {
	ref := func(name, target, desc string) ir.Field {
		return ir.Field{
			Name: name, Wire: wire(name),
			Type: target, TypeKind: ir.TypeKindObject, GoType: "*" + target,
			Modifiers:   []string{ir.ModifierNullable},
			Description: desc,
		}
	}

	return []ir.Field{
		ref("Equals", base+suffixFilterEquals, "Fields that must equal the given value."),
		ref("NotEquals", base+suffixFilterEquals, "Fields that must not equal the given value."),

		ref("GreaterThan", base+suffixFilterRange, "Fields that must be greater than the given value."),
		ref("SmallerThan", base+suffixFilterRange, "Fields that must be less than the given value."),
		ref("GreaterOrEqual", base+suffixFilterRange, "Fields that must be greater than or equal to the given value."),
		ref("SmallerOrEqual", base+suffixFilterRange, "Fields that must be less than or equal to the given value."),

		ref("Contains", base+suffixFilterContains, "Fields that must be one of the given values."),
		ref("NotContains", base+suffixFilterContains, "Fields that must not be one of the given values."),

		ref("Like", base+suffixFilterLike, "Text fields that must match the given pattern."),
		ref("NotLike", base+suffixFilterLike, "Text fields that must not match the given pattern."),

		ref("Null", base+suffixFilterNull, "Fields that must be null."),
		ref("NotNull", base+suffixFilterNull, "Fields that must not be null."),

		{
			Name: "OrCondition", Wire: wire("OrCondition"),
			Type: ir.TypeBool, TypeKind: ir.TypeKindPrimitive, GoType: "bool",
			Description: "Combine this filter's conditions with OR instead of AND.",
		},
		{
			Name: "NestedFilters", Wire: wire("NestedFilters"),
			Type: base + suffixFilter, TypeKind: ir.TypeKindObject, GoType: "[]" + base + suffixFilter,
			Modifiers: []string{ir.ModifierArray},
			Description: "Sub-filters combined with this one, so that AND and OR can be mixed " +
				"to any depth.",
		},
	}
}

// relationFilterFields adds one condition per relation, typed as the far side's
// object of the same kind.
//
// It is what lets a search reach across a foreign key — "fixtures whose home
// team is called Rovers" — without the caller running two queries and
// intersecting the answers themselves. A condition on a related row is written
// exactly where a condition on this row would be:
//
//	{"equals": {"kickoffAt": …, "homeTeam": {"name": "Rovers"}}}
//
// Conditions on the same relation from different operators are one question
// about one related row, not several: greaterThan.players.shirtNumber and
// equals.players.position together mean a single player who satisfies both.
//
// The types are mutually recursive, which is fine through a pointer and is also
// what makes the nesting unbounded: the repository refuses a filter nested
// deeper than it is willing to render, because a client that can ask for
// arbitrary depth can ask for an arbitrarily expensive query.
//
// A relation to a table the API does not expose is left out. The filter is the
// wire shape, and a condition on account_token's columns is a description of
// account_token published to anyone who can search accounts.
func relationFilterFields(res ir.Resource, wire func(string) string, exposed map[string]bool, suffix string, doc func(ir.Relation) string) []ir.Field {
	if res.Storage == nil {
		return nil
	}

	var out []ir.Field
	for _, rel := range res.Storage.Relations {
		if rel.Name == "" || rel.Target == "" || !exposed[rel.Target] {
			continue
		}
		// The snapshot self-reference is not a relation anybody is asking about.
		// Only a snapshot row has one, and every query-based read filters
		// snapshots out, so a condition on it could not match through any read
		// the API offers — the same reason the tenant is not in here.
		if snap := res.Storage.Snapshot; snap != nil && snap.FromID != nil &&
			rel.LocalColumn == snap.FromID.Name {
			continue
		}

		out = append(out, ir.Field{
			Name: rel.Name, Wire: wire(rel.Name),
			Type:        rel.Target + suffix,
			TypeKind:    ir.TypeKindObject,
			GoType:      "*" + rel.Target + suffix,
			Modifiers:   []string{ir.ModifierNullable},
			Description: doc(rel),
		})
	}
	return out
}

// relationFilterDoc says what matching means for this kind of relation, because
// the answer differs and the difference is easy to get wrong.
//
// The comment lands on a field that every operator shares, the negative ones
// included, so it has to say what a negative operator means here — and that is
// where a reader reasonably expects the wrong thing. NotEquals negates the
// comparison and still requires the related row; Without negates the row's
// existence. Saying so on the field is cheaper than finding out from a result
// set that was quietly a row short.
func relationFilterDoc(rel ir.Relation) string {
	switch rel.Kind {
	case ir.RelationBelongsTo:
		return "Conditions on this row's " + rel.Name + ".\n\n" +
			"A row with no " + rel.Name + " never matches, and that holds for the " +
			"negative operators too: under NotEquals this asks whether the " +
			rel.Name + " is there and differs, so a row without one fails that as " +
			"well. To ask the other question — no matching " + rel.Name + ", which " +
			"a row with none satisfies — use Without." + rel.Name + "."
	default:
		return "Conditions on one of this row's " + rel.Name + ".\n\n" +
			"Every condition has to hold of the same one, not of the set between " +
			"them.\n\n" +
			"Under the negative operators this asks whether one of them differs, " +
			"which is not the same as none of them matching: a row can have one " +
			"that matches and one that does not, satisfying the first and failing " +
			"the second. Without." + rel.Name + " asks the second."
	}
}

// relationAbsenceDoc says what "without" means for this kind of relation.
//
// The wording is deliberate about the empty case, because that is the question
// the operator objects cannot ask at all: a row with no related row satisfies
// every condition in here, including one naming a value it could never have had.
func relationAbsenceDoc(rel ir.Relation) string {
	switch rel.Kind {
	case ir.RelationBelongsTo:
		return "Match rows with no " + rel.Name + " satisfying these conditions, " +
			"including rows that have none at all.\n\n" +
			"This negates the existence of the related row rather than the " +
			"comparison, which is the whole difference from NotEquals." + rel.Name +
			": that one needs the " + rel.Name + " to be there and to differ, so a " +
			"row without one fails it and satisfies this. An empty object asks for " +
			"rows with no " + rel.Name + " at all."
	default:
		return "Match rows with no " + rel.Name + " satisfying these conditions, " +
			"including rows with none at all.\n\n" +
			"This negates the existence of a matching row rather than the " +
			"comparison, which is the whole difference from NotEquals." + rel.Name +
			": that one is satisfied by one of them differing, and a row can have " +
			"one that differs and one that matches at the same time. An empty " +
			"object asks for rows with no " + rel.Name + " at all."
	}
}

// isComparable reports whether a field supports ordering comparisons.
func isComparable(f ir.Field) bool {
	if f.IsArray() {
		return false
	}
	switch f.Type {
	case ir.TypeInt, ir.TypeInt64, ir.TypeFloat64, ir.TypeDecimal,
		ir.TypeDate, ir.TypeTime, ir.TypeTimestamp:
		return true
	default:
		return false
	}
}

// optional restates a field as a filter input: always nullable, since leaving a
// condition out is how you say you do not care about it.
func optional(f ir.Field, wire func(string) string) ir.Field {
	out := ir.Field{
		Name:        f.Name,
		Wire:        wire(f.Name),
		Type:        f.Type,
		TypeKind:    f.TypeKind,
		Format:      f.Format,
		Description: f.Description,
		Modifiers:   []string{ir.ModifierNullable},
	}
	out.GoType = pointerTo(f.GoType)
	if f.IsArray() {
		out.Modifiers = append(out.Modifiers, ir.ModifierArray)
		out.GoType = f.GoType
	}
	return out
}

// asArray restates a field as a list, for set-membership conditions.
func asArray(f ir.Field, wire func(string) string) ir.Field {
	return ir.Field{
		Name:        f.Name,
		Wire:        wire(f.Name),
		Type:        f.Type,
		TypeKind:    f.TypeKind,
		Format:      f.Format,
		Description: f.Description,
		Modifiers:   []string{ir.ModifierArray},
		GoType:      "[]" + baseGoType(f.GoType),
	}
}

// asBool restates a field as a presence flag: true means "must be null".
func asBool(f ir.Field, wire func(string) string) ir.Field {
	return ir.Field{
		Name:        f.Name,
		Wire:        wire(f.Name),
		Type:        ir.TypeBool,
		TypeKind:    ir.TypeKindPrimitive,
		GoType:      "*bool",
		Modifiers:   []string{ir.ModifierNullable},
		Description: f.Description,
	}
}

func pointerTo(goType string) string {
	if goType == "" {
		return ""
	}
	if goType[0] == '*' || goType[0] == '[' {
		return goType
	}
	return "*" + goType
}

// baseGoType strips a pointer or slice marker, so that a nullable column's
// element type is what ends up inside a list.
func baseGoType(goType string) string {
	for len(goType) > 0 && (goType[0] == '*') {
		goType = goType[1:]
	}
	if len(goType) > 2 && goType[:2] == "[]" {
		goType = goType[2:]
	}
	return goType
}
