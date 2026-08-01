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
func filterObjects(res ir.Resource, readable []ir.Field, wire func(string) string) []ir.Object {
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

		equals = append(equals, optional(f, wire))

		if isComparable(f) {
			ranged = append(ranged, optional(f, wire))
		}
		if !f.IsArray() {
			contains = append(contains, asArray(f, wire))
		}
		if f.Type == ir.TypeString {
			like = append(like, optional(f, wire))
		}
		if f.IsNullable() {
			null = append(null, asBool(f, wire))
		}
	}

	objs := []ir.Object{
		{
			Name:        base + suffixFilterEquals,
			Description: "Exact-match conditions on " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      equals,
		},
		{
			Name:        base + suffixFilterRange,
			Description: "Ordering conditions on " + res.Name + " fields that can be compared.",
			Origin:      ir.OriginFilter,
			Fields:      ranged,
		},
		{
			Name:        base + suffixFilterContains,
			Description: "Set-membership conditions on " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      contains,
		},
		{
			Name:        base + suffixFilterLike,
			Description: "Pattern conditions on " + res.Name + " text fields.",
			Origin:      ir.OriginFilter,
			Fields:      like,
		},
		{
			Name:        base + suffixFilterNull,
			Description: "Presence conditions on nullable " + res.Name + " fields.",
			Origin:      ir.OriginFilter,
			Fields:      null,
		},
		{
			Name:        base + suffixFilter,
			Description: "Conditions a " + res.Name + " must satisfy to match a search.",
			Origin:      ir.OriginFilter,
			Fields:      filterFields(base, wire),
		},
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
