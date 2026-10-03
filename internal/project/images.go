package project

import "os"

// The environment variables that name the containers `rig db up` starts.
//
// `database.image` and `database.electric.image` in rig.yaml are the project's
// answer, and they are the right place for it: one image, written down, the same
// on every machine. These exist for the case that file cannot express — an image
// whose *address* differs by who is pulling it.
//
// The case that forced them is a private registry. An image mirrored into a
// company's own ECR is reached at
// <account>.dkr.ecr.<region>.amazonaws.com/<repo>:<tag>, and the account is not
// a fact about the project: it is a fact about the deployment doing the pulling.
// Committing one into rig.yaml puts a second organisation's account in a file
// every laptop reads, and leaves a fork of the project pointing at a registry it
// cannot reach. It also stops being hypothetical the day an upstream image goes
// behind a login, which is what `electricsql/electric` did — every tag of it,
// not one.
//
// So the variable wins over the file. That is the opposite of how rig treats
// everything else, and the reason is that this is the one setting whose correct
// value is a property of the environment rather than of the project: a continuous
// integration job that has just logged in to a registry knows something rig.yaml
// cannot be told once and for all.
const (
	// ImageEnv overrides `database.image`.
	ImageEnv = "RIG_DB_IMAGE"
	// ElectricImageEnv overrides `database.electric.image`.
	ElectricImageEnv = "RIG_ELECTRIC_IMAGE"
)

// imageFromEnv is the image named by the environment, or empty.
//
// Empty and unset are one answer on purpose. A variable set to the empty string
// is a shell that expanded something to nothing — a step whose registry lookup
// failed, most often — and taking it literally would start a container named
// "", which fails later and further away than reading it as absent does.
func imageFromEnv(name string) string { return os.Getenv(name) }
