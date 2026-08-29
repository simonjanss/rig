// Package ir defines the intermediate representation that rig compiles a Postgres
// schema and its table configuration into, and that every generator reads.
//
// A [Document] holds two views of the same system:
//
//   - [Schema] is the physical truth: tables, columns, SQL types, keys, indexes.
//     It is what the database actually contains, normalized and sorted.
//   - [API] is the projected surface: resources, fields, endpoints, objects, enums.
//     It is what clients see.
//
// The two are cross-referenced. Every API [Field] backed by storage carries a
// [ColumnRef] naming the column behind it, so a generator emitting SQL and a
// generator emitting JSON cannot disagree about which column a field means.
// The reference is a denormalized copy of facts that also live in Schema;
// freezing a document asserts the copies match, which makes drift between the
// two views a compile error rather than a runtime surprise.
//
// Everything here is plain data. The package has no dependencies outside the
// standard library, imports nothing from the rest of rig, and performs no I/O.
// A Document is immutable once frozen: generators read it concurrently and must
// never write to it.
package ir
