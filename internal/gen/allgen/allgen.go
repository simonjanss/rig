// Package allgen imports every built-in generator for its registration.
//
// A generator registers itself from its own init, so adding one touches
// exactly one place: its own file, plus a line here. Nothing else has a list
// of generators to keep in step.
package allgen

import (
	_ "github.com/simonjanss/rig/internal/gen/electricgo"
	_ "github.com/simonjanss/rig/internal/gen/goclient"
	_ "github.com/simonjanss/rig/internal/gen/modelgo"
	_ "github.com/simonjanss/rig/internal/gen/persistgo"
	_ "github.com/simonjanss/rig/internal/gen/servergo"
	_ "github.com/simonjanss/rig/internal/gen/servicego"
)
