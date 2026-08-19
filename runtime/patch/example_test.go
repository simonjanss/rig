package patch_test

import (
	"encoding/json"
	"fmt"

	"github.com/simonjanss/rig/runtime/patch"
)

// A generated update input is a struct of these two types: Optional for a
// NOT NULL column, Nullable for one that allows null. Decoding a PATCH body
// into it is the whole point — a field the caller omitted is never touched, so
// it stays absent, while one they sent as null arrives as null.
func ExampleNullable() {
	// What a generated update input for a `todo` table looks like.
	var in struct {
		Title    patch.Optional[string] `json:"title"`
		Notes    patch.Nullable[string] `json:"notes"`
		Priority patch.Nullable[int]    `json:"priority"`
	}

	// The caller renamed the todo, cleared its notes, and said nothing at all
	// about its priority.
	body := `{"title": "Buy milk", "notes": null}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		panic(err)
	}

	fmt.Println("title:   ", in.Title, in.Title.IsSet())
	fmt.Println("notes:   ", in.Notes, in.Notes.IsNull())
	fmt.Println("priority:", in.Priority, in.Priority.Touched())

	// Output:
	// title:    Buy milk true
	// notes:    null true
	// priority: absent false
}

// Absent and null are different answers, and only one of them is a change. A
// repository asks Touched before it writes a column at all, and IsNull to
// decide whether it writes a value or a NULL.
func ExampleNullable_states() {
	set := patch.NewNullable("weekly")
	cleared := patch.Null[string]()
	untouched := patch.Unspecified[string]()

	for _, n := range []patch.Nullable[string]{set, cleared, untouched} {
		fmt.Printf("%-8s touched=%-5t null=%t\n", n, n.Touched(), n.IsNull())
	}

	// Output:
	// weekly   touched=true  null=false
	// null     touched=true  null=true
	// absent   touched=false null=false
}

// Optional has no third state, which is what makes clearing a NOT NULL column
// something that cannot be written down rather than something rejected at
// runtime.
func ExampleOptional() {
	title := patch.NewOptional("Buy milk")
	if v, ok := title.Get(); ok {
		fmt.Println("set to:", v)
	}
	fmt.Println("left alone:", patch.Absent[string]().IsAbsent())

	// Output:
	// set to: Buy milk
	// left alone: true
}

// FromPtr is how the current row — which a generated model holds as *T — becomes
// an input, so a partial update can be merged against the state it is patching.
func ExampleFromPtr() {
	current := "the previous note"

	fmt.Println(patch.FromPtr(&current))
	fmt.Println(patch.FromPtr[string](nil))

	// Output:
	// the previous note
	// null
}
