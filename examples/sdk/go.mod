module github.com/simonjanss/rig/examples/sdk

go 1.26.6

require (
	github.com/google/uuid v1.6.0
	github.com/simonjanss/rig/examples/auth v0.0.0
	github.com/simonjanss/rig/examples/todo v0.0.0
	github.com/simonjanss/rig/rigclient v0.0.0
	github.com/simonjanss/rig/runtime v0.1.0
)

replace github.com/simonjanss/rig/examples/auth => ../auth

replace github.com/simonjanss/rig/examples/todo => ../todo

replace github.com/simonjanss/rig/migrate => ../../migrate

replace github.com/simonjanss/rig/rigclient => ../../rigclient

replace github.com/simonjanss/rig/runtime => ../../runtime
