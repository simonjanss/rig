package ir

// CurrentVersion is the IR format version. Bump it when a change would make an
// older generator misread a newer document.
const CurrentVersion = 1

// Document is the complete compile output and the only input a generator reads.
//
// A document is immutable once produced. [Unmarshal] and the compiler's freeze
// step both index it eagerly, so concurrent readers are safe; mutating a
// document after that point is a bug.
type Document struct {
	IRVersion int    `json:"ir_version"`
	Tool      string `json:"generated_by"`

	API    API    `json:"api"`
	Schema Schema `json:"schema"`

	// Valid is false when the document was written despite validation errors.
	// Generators never run against an invalid document; it is emitted anyway so
	// that a broken project can still be inspected and repaired.
	Valid bool `json:"valid"`

	idx *index
}

// index is the lookup side table. Values are indices into the document's slices
// rather than pointers, so the index survives being built before the document
// is handed out and never aliases anything a caller could mutate.
type index struct {
	tables    map[string]int
	columns   map[[2]string]int // table name, column name
	enums     map[string]int
	pgEnums   map[string]int
	objects   map[string]int
	resources map[string]int
	// byTable maps a table name to the resource projected from it.
	resourceByTable map[string]int
}

// Reindex rebuilds the lookup tables. Call it after constructing a document by
// hand; [Unmarshal] and the compiler do it for you.
func (d *Document) Reindex() {
	ix := &index{
		tables:          make(map[string]int, len(d.Schema.Tables)),
		columns:         make(map[[2]string]int),
		enums:           make(map[string]int, len(d.API.Enums)),
		pgEnums:         make(map[string]int, len(d.Schema.Enums)),
		objects:         make(map[string]int, len(d.API.Objects)),
		resources:       make(map[string]int, len(d.API.Resources)),
		resourceByTable: make(map[string]int, len(d.API.Resources)),
	}
	for i := range d.Schema.Tables {
		t := &d.Schema.Tables[i]
		ix.tables[t.Name] = i
		for j := range t.Columns {
			ix.columns[[2]string{t.Name, t.Columns[j].Name}] = j
		}
	}
	for i := range d.Schema.Enums {
		ix.pgEnums[d.Schema.Enums[i].Name] = i
	}
	for i := range d.API.Enums {
		ix.enums[d.API.Enums[i].Name] = i
	}
	for i := range d.API.Objects {
		ix.objects[d.API.Objects[i].Name] = i
	}
	for i := range d.API.Resources {
		r := &d.API.Resources[i]
		ix.resources[r.Name] = i
		if r.Storage != nil {
			ix.resourceByTable[r.Storage.Table] = i
		}
	}
	d.idx = ix
}

// SetRevision stamps the API revision.
//
// It is the one field written after the compiler froze the document, and it has
// to be: the revision is decided from [Document.Hash], so the document has to
// exist before there is a revision to record. It is set once, before any
// generator runs, and nothing in the index refers to it — so the document is
// still safe to hand to concurrent readers afterwards.
func (d *Document) SetRevision(rev string) { d.API.Revision = rev }

func (d *Document) ensureIndex() *index {
	if d.idx == nil {
		d.Reindex()
	}
	return d.idx
}

// Table returns the named table, or nil.
func (d *Document) Table(name string) *Table {
	if i, ok := d.ensureIndex().tables[name]; ok {
		return &d.Schema.Tables[i]
	}
	return nil
}

// Column returns the named column of the named table, or nil.
func (d *Document) Column(table, column string) *Column {
	ix := d.ensureIndex()
	ti, ok := ix.tables[table]
	if !ok {
		return nil
	}
	ci, ok := ix.columns[[2]string{table, column}]
	if !ok {
		return nil
	}
	return &d.Schema.Tables[ti].Columns[ci]
}

// Resolve returns the column a reference points at, or nil. This is the hop
// from the denormalized [ColumnRef] carried on a field to the full [Column].
func (d *Document) Resolve(ref *ColumnRef) *Column {
	if ref == nil {
		return nil
	}
	return d.Column(ref.Table, ref.Name)
}

// PgEnum returns the named Postgres enum type, or nil.
func (d *Document) PgEnum(name string) *PgEnum {
	if i, ok := d.ensureIndex().pgEnums[name]; ok {
		return &d.Schema.Enums[i]
	}
	return nil
}

// Enum returns the named API enum, or nil.
func (d *Document) Enum(name string) *Enum {
	if i, ok := d.ensureIndex().enums[name]; ok {
		return &d.API.Enums[i]
	}
	return nil
}

// Object returns the named object, or nil.
func (d *Document) Object(name string) *Object {
	if i, ok := d.ensureIndex().objects[name]; ok {
		return &d.API.Objects[i]
	}
	return nil
}

// Resource returns the named resource, or nil.
func (d *Document) Resource(name string) *Resource {
	if i, ok := d.ensureIndex().resources[name]; ok {
		return &d.API.Resources[i]
	}
	return nil
}

// ResourceForTable returns the resource projected from the named table, or nil.
func (d *Document) ResourceForTable(table string) *Resource {
	if i, ok := d.ensureIndex().resourceByTable[table]; ok {
		return &d.API.Resources[i]
	}
	return nil
}

// TypeKindOf resolves a type name against this document.
func (d *Document) TypeKindOf(name string) (TypeKind, bool) {
	if IsPrimitive(name) {
		return TypeKindPrimitive, true
	}
	ix := d.ensureIndex()
	if _, ok := ix.enums[name]; ok {
		return TypeKindEnum, true
	}
	if _, ok := ix.resources[name]; ok {
		return TypeKindResource, true
	}
	if _, ok := ix.objects[name]; ok {
		return TypeKindObject, true
	}
	return "", false
}

// IsPrimitive reports whether name is one of the built-in field types.
func IsPrimitive(name string) bool {
	switch name {
	case TypeBool, TypeBytes, TypeDate, TypeDecimal, TypeFloat64,
		TypeInt, TypeInt64, TypeJSON, TypeString, TypeTime,
		TypeTimestamp, TypeUUID:
		return true
	default:
		return false
	}
}
